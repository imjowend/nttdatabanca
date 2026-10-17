package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type RAG struct {
	reg *Registry
	emb Embedder
	rr  Reranker // nil si no se va a rerankear (eval -norerank)
	qd  *Qdrant
	llm *LLM
	hoy time.Time // fecha de referencia (FECHA_REFERENCIA u hoy)

	dimMu sync.Mutex
	dimOK int // dimensión ya verificada contra la colección (0 = sin verificar)
}

// verificarDim compara (una vez) la dimensión del embedding de la query con la
// de la colección. Si la colección no existe no cachea nada: la query va a
// fallar con un mensaje claro.
func (r *RAG) verificarDim(ctx context.Context, n int) error {
	r.dimMu.Lock()
	defer r.dimMu.Unlock()
	if r.dimOK == n {
		return nil
	}
	dim, existe, err := r.qd.Info(ctx)
	if err != nil || !existe {
		return err
	}
	if dim != n {
		return errDim(r.qd.coll, dim, n)
	}
	r.dimOK = n
	return nil
}

func (r *RAG) referencia() time.Time {
	if r.hoy.IsZero() {
		return dia(time.Now())
	}
	return r.hoy
}

type Resultado struct {
	ChunkID   string  `json:"chunk_id"`
	DocID     string  `json:"doc_id"`
	Documento string  `json:"documento"`
	Tipo      string  `json:"tipo"`
	Estado    string  `json:"estado"`   // en la fecha de consulta
	Vigencia  string  `json:"vigencia"` // "01/07/2025 – indefinida"
	Ruta      string  `json:"ruta"`
	Seccion   string  `json:"seccion,omitempty"`
	Pagina    int     `json:"pagina"`
	URL       string  `json:"url,omitempty"`
	Texto     string  `json:"texto"`
	Score     float64 `json:"score"`
	// Motivo: por qué se agregó un fragmento que no vino del ranking
	// (reemplazo | modificacion | excepcion | definicion). Su score es 0.
	Motivo string `json:"motivo,omitempty"`
}

func (x Resultado) chunk() Chunk {
	return Chunk{ChunkID: x.ChunkID, DocID: x.DocID, Ruta: x.Ruta, Seccion: x.Seccion, Pagina: x.Pagina, Texto: x.Texto}
}

type ResultadoBusqueda struct {
	FechaConsulta string      `json:"fecha_consulta"`
	Resultados    []Resultado `json:"resultados"`
	Alertas       []Alerta    `json:"alertas"`
}

const candidatos = 30

func (r *RAG) aResultado(h Hit, p Periodo) Resultado {
	c := h.Payload
	res := Resultado{ChunkID: c.ChunkID, DocID: c.DocID, Documento: c.DocID, Ruta: c.Ruta, Seccion: c.Seccion,
		Pagina: c.Pagina, Texto: c.Texto, Score: h.Score, Estado: r.reg.Estado(c.DocID, p), Vigencia: r.reg.Vigencia(c.DocID)}
	if res.Estado == "modificado" && !r.reg.ArticuloModificado(c, p) {
		res.Estado = "vigente" // el documento tiene modificaciones, pero no en este artículo
	}
	if d, ok := r.reg.Get(c.DocID); ok {
		res.Documento, res.Tipo = d.Label(), d.Tipo
		if d.URL != "" {
			res.URL = d.URL
			if strings.HasSuffix(strings.ToLower(d.URL), ".pdf") && c.Pagina > 0 {
				res.URL = fmt.Sprintf("%s#page=%d", d.URL, c.Pagina)
			}
		}
	}
	return res
}

// Search: 1 embedding + 1 query híbrida (Qdrant) + 1 rerank, y después el
// contexto que el registro exige y el ranking puede no traer (reusando el mismo
// embedding, sin otra llamada al proveedor):
//   - reemplazo: si un fragmento no rige en el periodo, el artículo que lo
//     sustituye (por tabla de correspondencia) o fragmentos de su reemplazo;
//   - modificacion / excepcion: el documento que modifica o exceptúa ese artículo;
//   - definicion: la definición vigente de un término redefinido que el fragmento usa.
//
// Los que no rigen NO se filtran: se devuelven marcados con su estado.
func (r *RAG) Search(ctx context.Context, q string, k int, rerank bool, p Periodo) (*ResultadoBusqueda, error) {
	vec, err := r.emb.EmbedQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := r.verificarDim(ctx, len(vec)); err != nil {
		return nil, err
	}
	hits, err := r.qd.Hibrida(ctx, vec, bm25Query(tokenizar(q)), r.reg.DocIDsDesdeTexto(q), candidatos)
	if err != nil {
		return nil, err
	}

	if rerank && len(hits) > 0 {
		if r.rr == nil {
			return nil, fmt.Errorf("rerank pedido pero no hay Reranker configurado")
		}
		docs := make([]string, len(hits))
		for i, h := range hits {
			docs[i] = recortar(conEncabezado(r.reg, h.Payload), 4000)
		}
		rr, err := r.rr.Rerank(ctx, q, docs, min(k, len(hits)))
		if err != nil {
			return nil, err
		}
		orden := make([]Hit, 0, len(rr))
		for _, x := range rr {
			h := hits[x.Index]
			h.Score = x.Score
			orden = append(orden, h)
		}
		hits = orden
	} else if len(hits) > k {
		hits = hits[:k]
	}

	out := &ResultadoBusqueda{FechaConsulta: p.String()}
	presentes := map[string]bool{}   // chunk_id
	docPresente := map[string]bool{} // doc_id
	agregarHit := func(h Hit, motivo string) {
		if presentes[h.Payload.ChunkID] {
			return
		}
		res := r.aResultado(h, p)
		res.Motivo = motivo
		if motivo != "" {
			res.Score = 0
		}
		out.Resultados = append(out.Resultados, res)
		presentes[res.ChunkID], docPresente[res.DocID] = true, true
	}
	for _, h := range hits {
		agregarHit(h, "")
	}

	porID := map[string]string{}   // chunk_id → motivo
	porDoc := map[string]string{}  // doc_id → motivo (fragmentos por similitud dentro del doc)
	var ordenID, ordenDoc []string // orden de inserción, para resultados deterministas
	pedirID := func(id, motivo string) {
		if _, ya := porID[id]; !ya && !presentes[id] {
			porID[id] = motivo
			ordenID = append(ordenID, id)
		}
	}
	pedirDoc := func(id, motivo string) {
		if _, ya := porDoc[id]; !ya && !docPresente[id] {
			porDoc[id] = motivo
			ordenDoc = append(ordenDoc, id)
		}
	}
	for _, res := range out.Resultados {
		c := res.chunk()
		if !rige(res.Estado) {
			if dst, ok := r.reg.Correspondencia(res.ChunkID); ok {
				pedirID(dst, "reemplazo")
			} else {
				for _, x := range r.reg.Reemplazos(res.DocID) {
					pedirDoc(x, "reemplazo")
				}
			}
		}
		for _, a := range r.reg.Afectantes(c) {
			pedirDoc(a.Doc, a.Motivo)
		}
		for _, d := range r.reg.Definiciones(c, p) {
			pedirID(d, "definicion")
		}
	}
	if len(ordenID) > 0 {
		extra, err := r.qd.PorIDs(ctx, ordenID)
		if err != nil {
			log.Printf("search: no pude traer %v: %v", ordenID, err)
		}
		for _, h := range extra {
			agregarHit(h, porID[h.Payload.ChunkID])
		}
	}
	var faltan []string
	for _, d := range ordenDoc {
		if !docPresente[d] { // pudo llegar por ID en el paso anterior
			faltan = append(faltan, d)
		}
	}
	if len(faltan) > 0 {
		extra, err := r.qd.DenseEnDocs(ctx, vec, faltan, min(2*len(faltan), 6))
		if err != nil {
			log.Printf("search: no pude traer fragmentos de %v: %v", faltan, err)
		}
		for _, h := range extra {
			agregarHit(h, porDoc[h.Payload.DocID])
		}
	}

	out.Alertas = r.alertas(out.Resultados, p)
	return out, nil
}

// alertas: estado de cada documento (una vez) y lo que afecta a cada artículo.
func (r *RAG) alertas(res []Resultado, p Periodo) []Alerta {
	out := []Alerta{}
	vistas := map[Alerta]bool{}
	docs := map[string]bool{}
	for _, x := range res {
		var as []Alerta
		if !docs[x.DocID] {
			docs[x.DocID] = true
			as = r.reg.AlertasDoc(x.DocID, p)
		}
		for _, a := range append(as, r.reg.AlertasChunk(x.chunk(), p)...) {
			// La misma redefinición de un término sale una vez por documento.
			clave := Alerta{Tipo: a.Tipo, DocID: a.DocID, Detalle: a.Detalle}
			if !vistas[clave] {
				vistas[clave] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// notas: lo que el registro dice de este fragmento en el periodo, para el prompt.
func (r *RAG) notas(x Resultado, p Periodo) []string {
	var out []string
	for _, a := range append(r.reg.AlertasDoc(x.DocID, p), r.reg.AlertasChunk(x.chunk(), p)...) {
		out = append(out, a.Detalle)
	}
	return out
}

// ---------- Generación ----------

type Cita struct {
	ChunkID     string `json:"chunk_id"`
	CitaTextual string `json:"cita_textual"`
	Documento   string `json:"documento"`
	Ruta        string `json:"ruta"`
	Pagina      int    `json:"pagina"`
	Estado      string `json:"estado"`
	URL         string `json:"url,omitempty"`
}

type Contradiccion struct {
	ChunkA      string `json:"chunk_a"`
	ChunkB      string `json:"chunk_b"`
	Explicacion string `json:"explicacion"`
	Prevalece   string `json:"prevalece,omitempty"`
	Criterio    string `json:"criterio"`
}

type Respuesta struct {
	FechaConsulta    string `json:"fecha_consulta"`
	Respuesta        string `json:"respuesta"`
	SinRespuesta     bool   `json:"sin_respuesta"`
	Citas            []Cita `json:"citas"`
	CitasDescartadas int    `json:"citas_descartadas"`
	// CitasHistoricas: citas válidas de documentos que no rigen en la fecha de
	// consulta (derogados, caducados): se muestran como antecedente, no como norma.
	CitasHistoricas []Cita          `json:"citas_historicas"`
	Contradicciones []Contradiccion `json:"contradicciones"`
	Alertas         []Alerta        `json:"alertas"`
	Fuentes         []Resultado     `json:"fuentes"`
}

type salidaLLM struct {
	Respuesta    string `json:"respuesta"`
	SinRespuesta bool   `json:"sin_respuesta"`
	Citas        []struct {
		ChunkID     string `json:"chunk_id"`
		CitaTextual string `json:"cita_textual"`
	} `json:"citas"`
	Contradicciones []struct {
		ChunkA      string `json:"chunk_a"`
		ChunkB      string `json:"chunk_b"`
		Explicacion string `json:"explicacion"`
	} `json:"contradicciones"`
}

const promptSistema = `Eres un asistente de cumplimiento normativo para analistas de riesgo de un banco en Perú.
Responde ÚNICAMENTE con información de los FRAGMENTOS. No uses conocimiento externo.

Reglas:
1. Cada afirmación de "respuesta" termina con el chunk_id de su fuente entre corchetes, por ejemplo [prc-001#articulo-4].
2. En "citas", "cita_textual" es una copia EXACTA y literal (1 o 2 oraciones) del texto del fragmento. No parafrasees dentro de la cita.
3. Responde según la FECHA DE CONSULTA. Cada fragmento indica su vigencia y su estado en esa fecha, y puede traer NOTAS del registro de vigencia: las NOTAS son obligatorias y mandan sobre lo que diga el texto del fragmento (por ejemplo, un "Estado: VIGENTE" impreso en un documento que ya fue reemplazado).
4. Un fragmento DEROGADO, CADUCADO o AUN_NO_VIGENTE en la fecha de consulta no es norma aplicable: dilo explícitamente (por ejemplo "caducó el 31/12/2024 sin reemplazo") y responde con el documento que lo reemplaza si está entre los fragmentos. Si la pregunta es sobre una fecha pasada, aplica lo que regía en esa fecha.
5. Si una NOTA dice que un artículo fue modificado o que una excepción temporal prevalece sobre él, aplica el texto del documento modificatorio o de la excepción (dentro de su ámbito) y menciona también la regla general y cuándo vuelve a regir.
6. Si una NOTA dice que un término fue redefinido, aplica la definición nueva y advierte que el otro documento todavía conserva el texto anterior (indica qué dice cada uno).
7. Si dos fragmentos de documentos distintos fijan reglas incompatibles sobre lo mismo (plazos, montos, frecuencias, responsables, definiciones), repórtalo en "contradicciones".
8. Si los fragmentos no permiten responder, pon "sin_respuesta": true y explica qué falta. No inventes.

Devuelve SOLO un objeto JSON válido, sin texto adicional, con esta forma:
{"respuesta": "...", "sin_respuesta": false, "citas": [{"chunk_id": "...", "cita_textual": "..."}], "contradicciones": [{"chunk_a": "...", "chunk_b": "...", "explicacion": "..."}]}`

func (r *RAG) armarPrompt(pregunta string, p Periodo, res []Resultado) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FECHA DE CONSULTA: %s (fecha de referencia: %s)\nPREGUNTA: %s\n\nFRAGMENTOS:\n",
		p, fmtFecha(r.referencia()), pregunta)
	for _, x := range res {
		fmt.Fprintf(&b, "[%s]\nDocumento: %s | Tipo: %s | Vigencia: %s | Estado en la fecha de consulta: %s | %s | pág. %d\n",
			x.ChunkID, x.Documento, x.Tipo, x.Vigencia, strings.ToUpper(x.Estado), x.Ruta, x.Pagina)
		for _, n := range r.notas(x, p) {
			fmt.Fprintf(&b, "NOTA: %s\n", n)
		}
		fmt.Fprintf(&b, "Texto: %s\n---\n", x.Texto)
	}
	return b.String()
}

// Consulta es el body de /ask.
type Consulta struct {
	Pregunta  string  `json:"pregunta"`
	Fecha     string  `json:"fecha,omitempty"`     // YYYY-MM-DD o dd/mm/yyyy; pisa la fecha detectada en la pregunta
	Historial []Turno `json:"historial,omitempty"` // turnos anteriores, del más viejo al más nuevo
}

type Turno struct {
	Pregunta  string `json:"pregunta"`
	Respuesta string `json:"respuesta,omitempty"`
}

// Periodo de la consulta: fecha explícita > fecha en la pregunta > fecha en el
// turno anterior ("¿y quién lo aprueba?") > fecha de referencia.
func (r *RAG) Periodo(c Consulta) (Periodo, error) {
	if c.Fecha != "" {
		t, ok := parseFecha(c.Fecha)
		if !ok {
			return Periodo{}, fmt.Errorf("fecha %q inválida (YYYY-MM-DD o dd/mm/yyyy)", c.Fecha)
		}
		return periodoDia(t), nil
	}
	if p, ok := periodoDesdePregunta(c.Pregunta); ok {
		return p, nil
	}
	if n := len(c.Historial); n > 0 {
		if p, ok := periodoDesdePregunta(c.Historial[n-1].Pregunta); ok {
			return p, nil
		}
	}
	return periodoDia(r.referencia()), nil
}

// queryBusqueda: con historial, la pregunta anterior le da contexto a una
// repregunta corta ("¿Y en marzo de 2027?").
func queryBusqueda(c Consulta) string {
	if n := len(c.Historial); n > 0 {
		return c.Historial[n-1].Pregunta + " " + c.Pregunta
	}
	return c.Pregunta
}

// Ask: búsqueda + 1 sola llamada al LLM que responde Y detecta contradicciones
// (no hace falta una segunda llamada). Después el código valida todo.
func (r *RAG) Ask(ctx context.Context, c Consulta) (*Respuesta, error) {
	p, err := r.Periodo(c)
	if err != nil {
		return nil, err
	}
	sr, err := r.Search(ctx, queryBusqueda(c), 8, true, p)
	if err != nil {
		return nil, err
	}
	out := &Respuesta{FechaConsulta: p.String(), Alertas: sr.Alertas, Fuentes: sr.Resultados,
		Citas: []Cita{}, CitasHistoricas: []Cita{}, Contradicciones: []Contradiccion{}}
	if len(sr.Resultados) == 0 {
		out.SinRespuesta = true
		out.Respuesta = "No encontré fragmentos relevantes en el corpus."
		return out, nil
	}
	msgs := []Mensaje{{Role: "system", Content: promptSistema}}
	for _, t := range c.Historial {
		msgs = append(msgs, Mensaje{Role: "user", Content: t.Pregunta})
		if t.Respuesta != "" {
			msgs = append(msgs, Mensaje{Role: "assistant", Content: t.Respuesta})
		}
	}
	msgs = append(msgs, Mensaje{Role: "user", Content: r.armarPrompt(c.Pregunta, p, sr.Resultados)})
	raw, err := r.llm.Chat(ctx, msgs)
	if err != nil {
		return nil, err
	}
	var s salidaLLM
	if err := parsearJSONLaxo(raw, &s); err != nil {
		// Un solo reintento, con recordatorio explícito (cada llamada cuesta plata).
		log.Printf("ask: el LLM no devolvió JSON válido (%v); reintento una vez", err)
		msgs = append(msgs, Mensaje{Role: "assistant", Content: raw}, Mensaje{Role: "user", Content: recordatorioJSON})
		if raw, err = r.llm.Chat(ctx, msgs); err != nil {
			return nil, err
		}
		s = salidaLLM{}
		if err := parsearJSONLaxo(raw, &s); err != nil {
			return nil, fmt.Errorf("el LLM no devolvió JSON válido (tras 1 reintento): %w", err)
		}
	}
	out.Respuesta, out.SinRespuesta = s.Respuesta, s.SinRespuesta

	porID := map[string]Resultado{}
	for _, x := range sr.Resultados {
		porID[x.ChunkID] = x
	}
	var descartadas []string
	for _, ct := range s.Citas {
		x, ok := porID[ct.ChunkID]
		if !ok || !citaValida(x.Texto, ct.CitaTextual) {
			out.CitasDescartadas++ // cita alucinada o parafraseada: afuera
			motivo := "no coincide con el chunk"
			if !ok {
				motivo = "chunk_id inexistente"
			}
			descartadas = append(descartadas, fmt.Sprintf("[%s] %q (%s)", ct.ChunkID, recortar(ct.CitaTextual, 80), motivo))
			continue
		}
		// Se devuelve la cita ORIGINAL; la normalización es solo para comparar.
		cita := Cita{ChunkID: ct.ChunkID, CitaTextual: ct.CitaTextual,
			Documento: x.Documento, Ruta: x.Ruta, Pagina: x.Pagina, Estado: x.Estado, URL: x.URL}
		if rige(x.Estado) {
			out.Citas = append(out.Citas, cita)
		} else {
			out.CitasHistoricas = append(out.CitasHistoricas, cita)
		}
	}
	log.Printf("ask [%s]: citas válidas=%d históricas=%d descartadas=%d", p, len(out.Citas), len(out.CitasHistoricas), out.CitasDescartadas)
	for _, d := range descartadas {
		log.Printf("  cita descartada: %s", d)
	}
	for _, ct := range s.Contradicciones {
		a, okA := porID[ct.ChunkA]
		b, okB := porID[ct.ChunkB]
		if !okA || !okB || a.DocID == b.DocID {
			continue
		}
		gan, crit := r.reg.Prevalece(a.DocID, b.DocID, p)
		out.Contradicciones = append(out.Contradicciones, Contradiccion{
			ChunkA: ct.ChunkA, ChunkB: ct.ChunkB, Explicacion: ct.Explicacion, Prevalece: gan, Criterio: crit})
	}
	return out, nil
}

const recordatorioJSON = "Tu respuesta anterior no era JSON válido. Devolvé SOLO un objeto JSON válido con la forma " +
	"indicada en las instrucciones, sin texto adicional ni bloques de markdown."

func parsearJSONLaxo(raw string, v any) error {
	s := strings.TrimSpace(raw)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1] // tolera ```json ... ``` y texto alrededor
	}
	return json.Unmarshal([]byte(s), v)
}

var (
	comillas = strings.NewReplacer("“", `"`, "”", `"`, "«", `"`, "»", `"`, "‘", "'", "’", "'", "…", "...")
	// "regla-\n   mento" → "reglamento" (palabra cortada con guion al final de línea)
	reGuionCorte = regexp.MustCompile(`-\n\s*`)
)

// normCita: quita guiones de corte de línea, unifica comillas tipográficas,
// colapsa todo whitespace a un espacio, TrimSpace y ToLower.
func normCita(s string) string { return normCitaCon(s, "") }

// normCitaCon reemplaza el guion de corte de línea por sep: "" lo une
// ("regla-\nmento" → "reglamento") y "-" lo conserva ("socio-\neconómico" →
// "socio-económico"). Desde el texto no se sabe cuál de los dos es.
func normCitaCon(s, sep string) string {
	s = reGuionCorte.ReplaceAllString(s, sep)
	s = comillas.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizar = normCita + sin acentos (el LLM a veces los pierde o los agrega).
func normalizar(s string) string { return sinAcentos(normCita(s)) }

const (
	minCitaChars  = 20 // la cita completa (suma de tramos) debe tener al menos esto
	minTramoChars = 12 // con "...", cada tramo debe tener al menos esto
)

// citaValida verifica que la cita exista literalmente en el chunk, tolerando
// mayúsculas, acentos, comillas tipográficas, espacios y guiones de corte de
// línea. Con "..." la cita se parte en tramos: cada uno de ≥12 caracteres y
// todos en orden dentro del MISMO chunk.
func citaValida(fuente, cita string) bool {
	// El guion de fin de línea puede ser corte de palabra o guion real: se acepta
	// la cita si aparece en cualquiera de las dos lecturas de la fuente.
	return citaEn(normalizar(fuente), cita) ||
		citaEn(sinAcentos(normCitaCon(fuente, "-")), cita)
}

func citaEn(f, cita string) bool {
	var tramos []string
	for _, p := range strings.Split(normalizar(cita), "...") {
		if p = strings.Trim(p, ` .,;:"'`); p != "" {
			tramos = append(tramos, p)
		}
	}
	pos, total := 0, 0
	for _, p := range tramos {
		n := utf8.RuneCountInString(p)
		if len(tramos) > 1 && n < minTramoChars {
			return false
		}
		i := strings.Index(f[pos:], p)
		if i < 0 {
			return false
		}
		pos += i + len(p)
		total += n
	}
	return total >= minCitaChars
}
