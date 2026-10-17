package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

type RAG struct {
	reg *Registry
	emb Embedder
	rr  Reranker // nil si no se va a rerankear (eval -norerank)
	qd  *Qdrant
	llm *LLM

	// jina: atajo de compatibilidad (tests previos al refactor). Si emb/rr son
	// nil y jina no, se usa como Embedder/Reranker. El código nuevo usa emb/rr.
	jina *Jina

	dimMu sync.Mutex
	dimOK int // dimensión ya verificada contra la colección (0 = sin verificar)
}

func (r *RAG) embedder() Embedder {
	if r.emb == nil && r.jina != nil {
		return r.jina
	}
	return r.emb
}

func (r *RAG) reranker() Reranker {
	if r.rr == nil && r.jina != nil {
		return r.jina
	}
	return r.rr
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

type Resultado struct {
	ChunkID   string  `json:"chunk_id"`
	DocID     string  `json:"doc_id"`
	Documento string  `json:"documento"`
	Tipo      string  `json:"tipo"`
	Estado    string  `json:"estado"`
	Ruta      string  `json:"ruta"`
	Pagina    int     `json:"pagina"`
	URL       string  `json:"url,omitempty"`
	Texto     string  `json:"texto"`
	Score     float64 `json:"score"`
	Reemplazo bool    `json:"reemplazo,omitempty"` // agregado porque reemplaza a un derogado
}

type ResultadoBusqueda struct {
	Resultados []Resultado `json:"resultados"`
	Alertas    []Alerta    `json:"alertas"`
}

const candidatos = 30

func (r *RAG) aResultado(h Hit) Resultado {
	c := h.Payload
	res := Resultado{ChunkID: c.ChunkID, DocID: c.DocID, Documento: c.DocID, Ruta: c.Ruta,
		Pagina: c.Pagina, Texto: c.Texto, Score: h.Score, Estado: r.reg.Estado(c.DocID)}
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

// Search: 1 embedding + 1 query híbrida (Qdrant) + 1 rerank (proveedor configurado).
// Los derogados NO se filtran: se devuelven marcados y se trae su reemplazo.
func (r *RAG) Search(ctx context.Context, q string, k int, rerank bool) (*ResultadoBusqueda, error) {
	vec, err := r.embedder().EmbedQuery(ctx, q)
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
		rrk := r.reranker()
		if rrk == nil {
			return nil, errors.New("rerank pedido pero no hay Reranker configurado")
		}
		docs := make([]string, len(hits))
		for i, h := range hits {
			docs[i] = recortar(conEncabezado(r.reg, h.Payload), 4000)
		}
		rr, err := rrk.Rerank(ctx, q, docs, min(k, len(hits)))
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

	out := &ResultadoBusqueda{}
	presentes := map[string]bool{}
	for _, h := range hits {
		res := r.aResultado(h)
		out.Resultados = append(out.Resultados, res)
		presentes[res.DocID] = true
	}

	// Paso "agéntico" barato: si apareció un derogado y su reemplazo no está,
	// lo buscamos reusando el mismo embedding (sin otra llamada a Jina).
	var faltan []string
	for _, res := range out.Resultados {
		if res.Estado != "derogado" {
			continue
		}
		if s := r.reg.Vigente(res.DocID); !presentes[s] {
			presentes[s] = true
			faltan = append(faltan, s)
		}
	}
	if len(faltan) > 0 {
		if extra, err := r.qd.DenseEnDocs(ctx, vec, faltan, 2*len(faltan)); err == nil {
			for _, h := range extra {
				res := r.aResultado(h)
				res.Reemplazo = true
				out.Resultados = append(out.Resultados, res)
			}
		}
	}

	vistos := map[string]bool{}
	for _, res := range out.Resultados {
		if !vistos[res.DocID] {
			vistos[res.DocID] = true
			out.Alertas = append(out.Alertas, r.reg.Alertas(res.DocID)...)
		}
	}
	return out, nil
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
	Respuesta        string          `json:"respuesta"`
	SinRespuesta     bool            `json:"sin_respuesta"`
	Citas            []Cita          `json:"citas"`
	CitasDescartadas int             `json:"citas_descartadas"`
	Contradicciones  []Contradiccion `json:"contradicciones"`
	Alertas          []Alerta        `json:"alertas"`
	Fuentes          []Resultado     `json:"fuentes"`
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
1. Cada afirmación de "respuesta" termina con el chunk_id de su fuente entre corchetes, por ejemplo [sbs-877-2020#articulo-5].
2. En "citas", "cita_textual" es una copia EXACTA y literal (1 o 2 oraciones) del texto del fragmento. No parafrasees dentro de la cita.
3. Si un fragmento está DEROGADO, no lo presentes como vigente: indica que fue derogado y usa el documento que lo reemplaza si está entre los fragmentos.
4. Si dos fragmentos de documentos distintos fijan reglas incompatibles sobre lo mismo (plazos, montos, frecuencias, responsables), repórtalo en "contradicciones".
5. Si los fragmentos no permiten responder, pon "sin_respuesta": true y explica qué falta. No inventes.

Devuelve SOLO un objeto JSON válido, sin texto adicional, con esta forma:
{"respuesta": "...", "sin_respuesta": false, "citas": [{"chunk_id": "...", "cita_textual": "..."}], "contradicciones": [{"chunk_a": "...", "chunk_b": "...", "explicacion": "..."}]}`

func (r *RAG) armarPrompt(pregunta string, res []Resultado) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PREGUNTA: %s\n\nFRAGMENTOS:\n", pregunta)
	for _, x := range res {
		estado := strings.ToUpper(x.Estado)
		if d, ok := r.reg.Get(x.DocID); ok && d.DerogadoPor != "" {
			if s, ok := r.reg.Get(d.DerogadoPor); ok {
				estado += " (reemplazado por " + s.Label() + ")"
			}
		}
		fecha := ""
		if d, ok := r.reg.Get(x.DocID); ok {
			fecha = d.Fecha
		}
		fmt.Fprintf(&b, "[%s]\nDocumento: %s | Tipo: %s | Estado: %s | Fecha: %s | %s | pág. %d\nTexto: %s\n---\n",
			x.ChunkID, x.Documento, x.Tipo, estado, fecha, x.Ruta, x.Pagina, x.Texto)
	}
	return b.String()
}

// Ask: búsqueda + 1 sola llamada al LLM que responde Y detecta contradicciones
// (no hace falta una segunda llamada). Después el código valida todo.
func (r *RAG) Ask(ctx context.Context, pregunta string) (*Respuesta, error) {
	sr, err := r.Search(ctx, pregunta, 8, true)
	if err != nil {
		return nil, err
	}
	out := &Respuesta{Alertas: sr.Alertas, Fuentes: sr.Resultados, Citas: []Cita{}, Contradicciones: []Contradiccion{}}
	if len(sr.Resultados) == 0 {
		out.SinRespuesta = true
		out.Respuesta = "No encontré fragmentos relevantes en el corpus."
		return out, nil
	}
	msgs := []Mensaje{{Role: "system", Content: promptSistema}, {Role: "user", Content: r.armarPrompt(pregunta, sr.Resultados)}}
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
	for _, c := range s.Citas {
		x, ok := porID[c.ChunkID]
		if !ok || !citaValida(x.Texto, c.CitaTextual) {
			out.CitasDescartadas++ // cita alucinada o parafraseada: afuera
			motivo := "no coincide con el chunk"
			if !ok {
				motivo = "chunk_id inexistente"
			}
			descartadas = append(descartadas, fmt.Sprintf("[%s] %q (%s)", c.ChunkID, recortar(c.CitaTextual, 80), motivo))
			continue
		}
		// Se devuelve la cita ORIGINAL; la normalización es solo para comparar.
		out.Citas = append(out.Citas, Cita{ChunkID: c.ChunkID, CitaTextual: c.CitaTextual,
			Documento: x.Documento, Ruta: x.Ruta, Pagina: x.Pagina, Estado: x.Estado, URL: x.URL})
	}
	log.Printf("ask: citas válidas=%d descartadas=%d", len(out.Citas), out.CitasDescartadas)
	for _, d := range descartadas {
		log.Printf("  cita descartada: %s", d)
	}
	for _, c := range s.Contradicciones {
		a, okA := porID[c.ChunkA]
		b, okB := porID[c.ChunkB]
		if !okA || !okB || a.DocID == b.DocID {
			continue
		}
		gan, crit := r.reg.Prevalece(a.DocID, b.DocID)
		out.Contradicciones = append(out.Contradicciones, Contradiccion{
			ChunkA: c.ChunkA, ChunkB: c.ChunkB, Explicacion: c.Explicacion, Prevalece: gan, Criterio: crit})
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
