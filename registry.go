package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Doc es una fila del registro de vigencia. Es la ÚNICA fuente de verdad sobre
// el estado de cada documento: no se guarda en Qdrant, así que cambiar una
// relación no obliga a re-indexar (solo reiniciar el server).
//
// Las relaciones se cargan en el documento que actúa (lo que dice su PDF:
// "Reemplaza a", "Modifica a"...); las inversas se calculan al cargar.
type Doc struct {
	ID           string `json:"doc_id"`
	Numero       string `json:"numero,omitempty"` // código del documento, p. ej. "PRC-001"
	Titulo       string `json:"titulo"`
	Tipo         string `json:"tipo,omitempty"` // "Política interna", "Circular temporal"...
	Archivo      string `json:"archivo,omitempty"`
	URL          string `json:"url,omitempty"`
	Padre        string `json:"padre,omitempty"`         // documento que lo contiene (anexo con metadatos propios)
	Fecha        string `json:"fecha,omitempty"`         // aprobación, YYYY-MM-DD
	VigenteDesde string `json:"vigente_desde,omitempty"` // YYYY-MM-DD
	VigenteHasta string `json:"vigente_hasta,omitempty"` // YYYY-MM-DD; vacío = indefinido

	Reemplaza       []string          `json:"reemplaza,omitempty"`        // deroga / deja sin efecto, desde su vigente_desde
	Modifica        []RelArt          `json:"modifica,omitempty"`         // cambia artículos puntuales, desde su vigente_desde
	PrevaleceSobre  []RelArt          `json:"prevalece_sobre,omitempty"`  // excepción: solo mientras este documento rige
	Redefine        []Redefinicion    `json:"redefine,omitempty"`         // cambia el sentido de un término usado en otros
	Correspondencia map[string]string `json:"correspondencias,omitempty"` // chunk de origen → chunk de destino
}

// RelArt apunta a artículos de otro documento (vacío = todo el documento).
type RelArt struct {
	Doc       string `json:"doc"`
	Articulos []int  `json:"articulos,omitempty"`
}

type Redefinicion struct {
	Termino string   `json:"termino"`
	Chunk   string   `json:"chunk"`          // chunk con la definición nueva
	Docs    []string `json:"docs,omitempty"` // documentos que usan el término (informativo)
}

func (d *Doc) Label() string {
	if d.Numero == "" {
		return d.Titulo
	}
	return fmt.Sprintf("%s (%s)", d.Titulo, d.Numero)
}

// Codigo devuelve el número o, si no hay, el doc_id.
func (d *Doc) Codigo() string {
	if d.Numero != "" {
		return d.Numero
	}
	return d.ID
}

func (d *Doc) desde() (time.Time, bool) { return parseFecha(d.VigenteDesde) }
func (d *Doc) hasta() (time.Time, bool) { return parseFecha(d.VigenteHasta) }

// afecta: relación inversa de Modifica / PrevaleceSobre.
type afecta struct {
	Por       string
	Articulos []int
}

func (a afecta) cubre(art int) bool {
	return len(a.Articulos) == 0 || slices.Contains(a.Articulos, art)
}

type Registry struct {
	docs     map[string]*Doc
	orden    []*Doc
	byNumero map[string]string

	reemplazadoPor map[string][]string
	modificadoPor  map[string][]afecta
	exceptuadoPor  map[string][]afecta
	redefs         []redefPor
	corresp        map[string]string
}

type redefPor struct {
	Redefinicion
	Por  string
	toks []string // término tokenizado (sin plurales ni stopwords), para buscarlo en textos
}

// usaTermino: el texto menciona el término ("Clientes de Alto Riesgo" cuenta).
func usaTermino(texto string, termino []string) bool {
	t := tokenizar(texto)
	for i := 0; i+len(termino) <= len(t); i++ {
		if slices.Equal(t[i:i+len(termino)], termino) {
			return true
		}
	}
	return false
}

func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var docs []*Doc
	if err := json.Unmarshal(data, &docs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r, err := nuevoRegistry(docs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func nuevoRegistry(docs []*Doc) (*Registry, error) {
	r := &Registry{docs: map[string]*Doc{}, byNumero: map[string]string{}, reemplazadoPor: map[string][]string{},
		modificadoPor: map[string][]afecta{}, exceptuadoPor: map[string][]afecta{}, corresp: map[string]string{}}
	for _, d := range docs {
		if d.ID == "" {
			return nil, fmt.Errorf("documento sin doc_id: %q", d.Titulo)
		}
		if _, dup := r.docs[d.ID]; dup {
			return nil, fmt.Errorf("doc_id duplicado: %s", d.ID)
		}
		for _, f := range []string{d.Fecha, d.VigenteDesde, d.VigenteHasta} {
			if _, ok := parseFecha(f); f != "" && !ok {
				return nil, fmt.Errorf("%s: fecha inválida %q (usar YYYY-MM-DD)", d.ID, f)
			}
		}
		r.docs[d.ID] = d
		r.orden = append(r.orden, d)
		if d.Numero != "" {
			r.byNumero[strings.ToUpper(d.Numero)] = d.ID
		}
	}
	existe := func(de, ref string) error {
		if _, ok := r.docs[ref]; !ok {
			return fmt.Errorf("%s referencia a %q, que no está en el registro", de, ref)
		}
		return nil
	}
	for _, d := range docs {
		if d.Padre != "" {
			if err := existe(d.ID, d.Padre); err != nil {
				return nil, err
			}
			if p := r.docs[d.Padre]; d.VigenteDesde == "" {
				d.VigenteDesde = p.VigenteDesde // hereda la vigencia del documento que lo contiene
			}
		}
		for _, x := range d.Reemplaza {
			if err := existe(d.ID, x); err != nil {
				return nil, err
			}
			r.reemplazadoPor[x] = append(r.reemplazadoPor[x], d.ID)
		}
		for _, m := range d.Modifica {
			if err := existe(d.ID, m.Doc); err != nil {
				return nil, err
			}
			r.modificadoPor[m.Doc] = append(r.modificadoPor[m.Doc], afecta{d.ID, m.Articulos})
		}
		for _, p := range d.PrevaleceSobre {
			if err := existe(d.ID, p.Doc); err != nil {
				return nil, err
			}
			r.exceptuadoPor[p.Doc] = append(r.exceptuadoPor[p.Doc], afecta{d.ID, p.Articulos})
		}
		for _, rd := range d.Redefine {
			if docDeChunk(rd.Chunk) != d.ID {
				return nil, fmt.Errorf("%s redefine %q con el chunk %q, que no es suyo", d.ID, rd.Termino, rd.Chunk)
			}
			r.redefs = append(r.redefs, redefPor{rd, d.ID, tokenizar(rd.Termino)})
		}
		for o, dst := range d.Correspondencia {
			for _, c := range []string{o, dst} {
				if err := existe(d.ID, docDeChunk(c)); err != nil {
					return nil, err
				}
			}
			r.corresp[o] = dst
		}
	}
	return r, nil
}

func docDeChunk(chunkID string) string { id, _, _ := strings.Cut(chunkID, "#"); return id }

var reNumArticulo = regexp.MustCompile(`^Artículo (\d+)\b`)

// numeroArticulo: "Artículo 4 (parte 2)" → 4; 0 si la ruta no es un artículo numerado.
func numeroArticulo(ruta string) int {
	if m := reNumArticulo.FindStringSubmatch(ruta); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func (r *Registry) Get(id string) (*Doc, bool) { d, ok := r.docs[id]; return d, ok }
func (r *Registry) Docs() []*Doc               { return r.orden }

// Reemplazos: documentos que dejaron sin efecto a id (directos).
func (r *Registry) Reemplazos(id string) []string { return r.reemplazadoPor[id] }

// finPorReemplazo: desde cuándo id deja de regir porque otro lo reemplazó
// (el primer vigente_desde de sus reemplazos). ok=false si nadie lo reemplaza;
// un reemplazo sin fecha lo deja sin efecto desde siempre.
func (r *Registry) finPorReemplazo(id string) (time.Time, bool) {
	var fin time.Time
	ok := false
	for _, x := range r.reemplazadoPor[id] {
		t, conFecha := r.docs[x].desde()
		if !conFecha {
			return time.Time{}, true
		}
		if !ok || t.Before(fin) {
			fin, ok = t, true
		}
	}
	return fin, ok
}

// EnVigor: id rige en algún momento del periodo.
func (r *Registry) EnVigor(id string, p Periodo) bool { return rige(r.Estado(id, p)) }

func rige(estado string) bool {
	return estado == "vigente" || estado == "vigente_temporal" || estado == "modificado"
}

// Estado en el periodo: derogado | caducado | aun_no_vigente | vigente_temporal | modificado | vigente.
// Si el periodo abarca un cambio (p. ej. "en 2025" para PGR-001), cuenta como vigente:
// el prompt lleva las fechas para que la respuesta las distinga.
func (r *Registry) Estado(id string, p Periodo) string {
	d, ok := r.docs[id]
	if !ok {
		return "desconocido"
	}
	if fin, ok := r.finPorReemplazo(id); ok && !fin.After(p.Desde) {
		return "derogado"
	}
	if h, ok := d.hasta(); ok && h.Before(p.Desde) {
		return "caducado"
	}
	if t, ok := d.desde(); ok && t.After(p.Hasta) {
		return "aun_no_vigente"
	}
	if _, ok := d.hasta(); ok {
		return "vigente_temporal"
	}
	for _, m := range r.modificadoPor[id] {
		if t, ok := r.docs[m.Por].desde(); !ok || !t.After(p.Hasta) {
			return "modificado"
		}
	}
	return "vigente"
}

// Vigencia en texto: "01/07/2025 – indefinida".
func (r *Registry) Vigencia(id string) string {
	d, ok := r.docs[id]
	if !ok || d.VigenteDesde == "" {
		return "sin dato"
	}
	hasta := "indefinida"
	if d.VigenteHasta != "" {
		hasta = fmtISO(d.VigenteHasta)
	}
	if fin, ok := r.finPorReemplazo(id); ok && !fin.IsZero() {
		hasta = "sin efecto desde " + fmtFecha(fin)
	}
	return fmtISO(d.VigenteDesde) + " – " + hasta
}

type Alerta struct {
	Tipo    string `json:"tipo"` // derogado | caducado | aun_no_vigente | temporal | modificado | modificacion_posterior | excepcion | excepcion_vencida | redefinido
	DocID   string `json:"doc_id"`
	ChunkID string `json:"chunk_id,omitempty"` // si la alerta es de un artículo
	Detalle string `json:"detalle"`
}

func (r *Registry) labels(ids []string) string {
	var ls []string
	for _, id := range ids {
		ls = append(ls, r.docs[id].Label())
	}
	return strings.Join(ls, "; ")
}

func artTxt(art int) string {
	if art == 0 {
		return ""
	}
	return fmt.Sprintf(" Art. %d°", art)
}

// AlertasDoc: estado del documento en el periodo. Determinístico, sin LLM.
func (r *Registry) AlertasDoc(id string, p Periodo) []Alerta {
	d, ok := r.docs[id]
	if !ok {
		return nil
	}
	switch r.Estado(id, p) {
	case "derogado":
		fin, _ := r.finPorReemplazo(id)
		cuando := ""
		if !fin.IsZero() {
			cuando = " desde el " + fmtFecha(fin)
		}
		return []Alerta{{Tipo: "derogado", DocID: id, Detalle: fmt.Sprintf(
			"%s no está vigente: quedó sin efecto%s, reemplazado por %s. No usar como norma vigente.",
			d.Label(), cuando, r.labels(r.reemplazadoPor[id]))}}
	case "caducado":
		return []Alerta{{Tipo: "caducado", DocID: id, Detalle: fmt.Sprintf(
			"%s caducó el %s y no tiene documento de reemplazo. Solo vale como consulta histórica.",
			d.Label(), fmtISO(d.VigenteHasta))}}
	case "aun_no_vigente":
		return []Alerta{{Tipo: "aun_no_vigente", DocID: id, Detalle: fmt.Sprintf(
			"%s todavía no regía en esa fecha: entra en vigencia el %s.", d.Label(), fmtISO(d.VigenteDesde))}}
	case "vigente_temporal":
		det := fmt.Sprintf("%s es temporal: rige del %s al %s.", d.Label(), fmtISO(d.VigenteDesde), fmtISO(d.VigenteHasta))
		var vuelve []string
		for _, ps := range d.PrevaleceSobre {
			vuelve = append(vuelve, r.docs[ps.Doc].Codigo()+artsTxt(ps.Articulos))
		}
		if len(vuelve) > 0 {
			det += " Al vencer vuelve a aplicarse " + strings.Join(vuelve, ", ") + "."
		}
		return []Alerta{{Tipo: "temporal", DocID: id, Detalle: det}}
	}
	return nil
}

func artsTxt(arts []int) string {
	if len(arts) == 0 {
		return ""
	}
	var s []string
	for _, a := range arts {
		s = append(s, fmt.Sprintf("%d°", a))
	}
	return " Art. " + strings.Join(s, ", ")
}

// AlertasChunk: lo que afecta a un artículo puntual en el periodo:
// modificaciones, excepciones temporales y términos redefinidos.
func (r *Registry) AlertasChunk(c Chunk, p Periodo) []Alerta {
	d, ok := r.docs[c.DocID]
	if !ok {
		return nil
	}
	art := numeroArticulo(c.Ruta)
	var out []Alerta
	if art > 0 {
		for _, m := range r.modificadoPor[c.DocID] {
			if !m.cubre(art) {
				continue
			}
			md := r.docs[m.Por]
			if t, ok := md.desde(); ok && t.After(p.Hasta) {
				out = append(out, Alerta{"modificacion_posterior", c.DocID, c.ChunkID, fmt.Sprintf(
					"En esa fecha regía este texto de %s%s; fue modificado después por %s, desde el %s.",
					d.Codigo(), artTxt(art), md.Label(), fmtFecha(t))})
				continue
			}
			out = append(out, Alerta{"modificado", c.DocID, c.ChunkID, fmt.Sprintf(
				"%s%s fue modificado por %s (vigente desde %s): usar el texto modificado, no el de este fragmento.",
				d.Codigo(), artTxt(art), md.Label(), fmtISO(md.VigenteDesde))})
		}
		for _, e := range r.exceptuadoPor[c.DocID] {
			if !e.cubre(art) {
				continue
			}
			ed := r.docs[e.Por]
			switch est := r.Estado(e.Por, p); {
			case rige(est):
				out = append(out, Alerta{"excepcion", c.DocID, c.ChunkID, fmt.Sprintf(
					"Mientras rige %s (%s), prevalece sobre %s%s dentro de su ámbito.",
					ed.Label(), r.Vigencia(e.Por), d.Codigo(), artTxt(art))})
			case est == "caducado":
				out = append(out, Alerta{"excepcion_vencida", c.DocID, c.ChunkID, fmt.Sprintf(
					"La excepción %s venció el %s: en esa fecha rige %s%s.",
					ed.Label(), fmtISO(ed.VigenteHasta), d.Codigo(), artTxt(art))})
			}
		}
	}
	for _, rd := range r.redefs {
		if rd.Por == c.DocID || !usaTermino(c.Texto, rd.toks) || !r.EnVigor(rd.Por, p) {
			continue
		}
		out = append(out, Alerta{"redefinido", c.DocID, c.ChunkID, fmt.Sprintf(
			"El término «%s» se interpreta según %s (vigente desde %s), que prevalece sobre la definición de %s; "+
				"este documento conserva el texto anterior.", rd.Termino, r.docs[rd.Por].Label(), fmtISO(r.docs[rd.Por].VigenteDesde), d.Codigo())})
	}
	return out
}

// Correspondencia: el chunk que reemplaza a chunkID según una tabla de distribución.
func (r *Registry) Correspondencia(chunkID string) (string, bool) {
	c, ok := r.corresp[chunkID]
	return c, ok
}

// Definiciones: chunks con la definición vigente de términos que c usa y que
// otro documento redefinió (para traerlos al contexto).
func (r *Registry) Definiciones(c Chunk, p Periodo) []string {
	var out []string
	for _, rd := range r.redefs {
		if rd.Chunk != c.ChunkID && usaTermino(c.Texto, rd.toks) && r.EnVigor(rd.Por, p) {
			out = append(out, rd.Chunk)
		}
	}
	return out
}

// ArticuloModificado: alguna modificación vigente en el periodo toca el artículo de c.
func (r *Registry) ArticuloModificado(c Chunk, p Periodo) bool {
	art := numeroArticulo(c.Ruta)
	for _, m := range r.modificadoPor[c.DocID] {
		if t, ok := r.docs[m.Por].desde(); m.cubre(art) && art > 0 && (!ok || !t.After(p.Hasta)) {
			return true
		}
	}
	return false
}

type Afectante struct{ Doc, Motivo string } // Motivo: modificacion | excepcion

// Afectantes: documentos que modifican o exceptúan el artículo de c.
func (r *Registry) Afectantes(c Chunk) []Afectante {
	art := numeroArticulo(c.Ruta)
	if art == 0 {
		return nil
	}
	var out []Afectante
	for motivo, rels := range map[string][]afecta{"modificacion": r.modificadoPor[c.DocID], "excepcion": r.exceptuadoPor[c.DocID]} {
		for _, a := range rels {
			if a.cubre(art) {
				out = append(out, Afectante{a.Por, motivo})
			}
		}
	}
	slices.SortFunc(out, func(a, b Afectante) int { return strings.Compare(a.Doc, b.Doc) })
	return out
}

type Historial struct {
	Doc            *Doc     `json:"doc"`
	Estado         string   `json:"estado"`
	Vigencia       string   `json:"vigencia"`
	Reemplaza      []string `json:"reemplaza"`
	ReemplazadoPor []string `json:"reemplazado_por"`
	Modifica       []string `json:"modifica"`
	ModificadoPor  []string `json:"modificado_por"`
	PrevaleceSobre []string `json:"prevalece_sobre"`
	ExceptuadoPor  []string `json:"exceptuado_por"`
	Derivados      []string `json:"derivados"`
}

func (r *Registry) Historial(id string, p Periodo) (*Historial, bool) {
	d, ok := r.docs[id]
	if !ok {
		return nil, false
	}
	h := &Historial{Doc: d, Estado: r.Estado(id, p), Vigencia: r.Vigencia(id),
		Reemplaza: d.Reemplaza, ReemplazadoPor: r.reemplazadoPor[id]}
	for _, m := range d.Modifica {
		h.Modifica = append(h.Modifica, m.Doc+artsTxt(m.Articulos))
	}
	for _, m := range r.modificadoPor[id] {
		h.ModificadoPor = append(h.ModificadoPor, m.Por+artsTxt(m.Articulos))
	}
	for _, e := range d.PrevaleceSobre {
		h.PrevaleceSobre = append(h.PrevaleceSobre, e.Doc+artsTxt(e.Articulos))
	}
	for _, e := range r.exceptuadoPor[id] {
		h.ExceptuadoPor = append(h.ExceptuadoPor, e.Por+artsTxt(e.Articulos))
	}
	for _, o := range r.orden {
		if o.Padre == id {
			h.Derivados = append(h.Derivados, o.ID)
		}
	}
	return h, true
}

// Prevalece decide SIN LLM qué documento manda ante una contradicción en el
// periodo: el LLM detecta el conflicto, el código decide quién gana.
func (r *Registry) Prevalece(a, b string, p Periodo) (ganador, criterio string) {
	da, db := r.docs[a], r.docs[b]
	if da == nil || db == nil {
		return "", "indeterminado"
	}
	enA, enB := r.EnVigor(a, p), r.EnVigor(b, p)
	if enA != enB {
		if enA {
			return a, "el otro documento no rige en la fecha de consulta (" + r.Estado(b, p) + ")"
		}
		return b, "el otro documento no rige en la fecha de consulta (" + r.Estado(a, p) + ")"
	}
	if c := r.relacion(a, b); c != "" {
		return a, c
	}
	if c := r.relacion(b, a); c != "" {
		return b, c
	}
	ta, okA := da.desde()
	tb, okB := db.desde()
	if okA && okB && !ta.Equal(tb) {
		if ta.After(tb) {
			return a, "norma posterior"
		}
		return b, "norma posterior"
	}
	return "", "indeterminado: requiere revisión humana"
}

// relacion: criterio por el que a prevalece expresamente sobre b ("" si no hay).
func (r *Registry) relacion(a, b string) string {
	da := r.docs[a]
	for _, x := range da.PrevaleceSobre {
		if x.Doc == b {
			return "excepción expresa y vigente (" + da.Codigo() + ")"
		}
	}
	for _, rd := range da.Redefine {
		if slices.Contains(rd.Docs, b) {
			return "prevalencia expresa de la definición de «" + rd.Termino + "» (" + da.Codigo() + ")"
		}
	}
	for _, x := range da.Modifica {
		if x.Doc == b {
			return "modificación expresa (" + da.Codigo() + ")"
		}
	}
	if slices.Contains(da.Reemplaza, b) {
		return "lo reemplaza expresamente (" + da.Codigo() + ")"
	}
	return ""
}

// DocIDsDesdeTexto devuelve los documentos mencionados por código en la pregunta.
func (r *Registry) DocIDsDesdeTexto(q string) []string {
	var out []string
	for _, m := range reCodigo.FindAllString(strings.ToUpper(q), -1) {
		if id, ok := r.byNumero[m]; ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
