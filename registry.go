package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Doc es una fila del registro de vigencia. Es la ÚNICA fuente de verdad sobre
// el estado de cada documento: no se guarda en Qdrant, así que cargar una
// derogación nueva no obliga a re-indexar (solo reiniciar el server).
type Doc struct {
	ID            string   `json:"doc_id"`
	Numero        string   `json:"numero,omitempty"`
	Titulo        string   `json:"titulo"`
	Tipo          string   `json:"tipo"` // norma_sbs | basilea | politica_interna | procedimiento_interno
	Archivo       string   `json:"archivo,omitempty"`
	URL           string   `json:"url,omitempty"`
	Fecha         string   `json:"fecha,omitempty"`         // emisión, YYYY-MM-DD
	VigenteDesde  string   `json:"vigente_desde,omitempty"` // YYYY-MM-DD
	DerogadoPor   string   `json:"derogado_por,omitempty"`
	ModificadoPor []string `json:"modificado_por,omitempty"`
}

func (d *Doc) Label() string {
	if d.Numero == "" {
		return d.Titulo
	}
	return fmt.Sprintf("%s (%s)", d.Titulo, d.Numero)
}

type Registry struct {
	docs     map[string]*Doc
	orden    []*Doc
	byNumero map[string]string
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
	r := &Registry{docs: map[string]*Doc{}, byNumero: map[string]string{}}
	for _, d := range docs {
		if d.ID == "" {
			return nil, fmt.Errorf("documento sin doc_id: %q", d.Titulo)
		}
		if _, dup := r.docs[d.ID]; dup {
			return nil, fmt.Errorf("doc_id duplicado: %s", d.ID)
		}
		r.docs[d.ID] = d
		r.orden = append(r.orden, d)
		completo, pelado := clavesNumero(d.Numero)
		if completo != "" {
			r.byNumero[completo] = d.ID
		}
		if _, existe := r.byNumero[pelado]; pelado != "" && !existe {
			r.byNumero[pelado] = d.ID
		}
	}
	// Validación de integridad: toda referencia tiene que existir.
	for _, d := range docs {
		refs := append([]string{}, d.ModificadoPor...)
		if d.DerogadoPor != "" {
			refs = append(refs, d.DerogadoPor)
		}
		for _, ref := range refs {
			if _, ok := r.docs[ref]; !ok {
				return nil, fmt.Errorf("%s referencia a %q, que no está en el registro", d.ID, ref)
			}
		}
	}
	return r, nil
}

func (r *Registry) Get(id string) (*Doc, bool) { d, ok := r.docs[id]; return d, ok }
func (r *Registry) Docs() []*Doc               { return r.orden }

func (r *Registry) Estado(id string) string {
	d, ok := r.docs[id]
	switch {
	case !ok:
		return "desconocido"
	case d.DerogadoPor != "":
		return "derogado"
	case len(d.ModificadoPor) > 0:
		return "modificado"
	default:
		return "vigente"
	}
}

// Vigente sigue la cadena derogado_por hasta el documento que rige hoy.
func (r *Registry) Vigente(id string) string {
	for i := 0; i < 20; i++ {
		d, ok := r.docs[id]
		if !ok || d.DerogadoPor == "" {
			return id
		}
		id = d.DerogadoPor
	}
	return id
}

type Alerta struct {
	Tipo    string `json:"tipo"` // derogado | modificado
	DocID   string `json:"doc_id"`
	Detalle string `json:"detalle"`
}

// Alertas es determinístico: no usa LLM, sale del registro.
func (r *Registry) Alertas(id string) []Alerta {
	d, ok := r.docs[id]
	if !ok {
		return nil
	}
	var out []Alerta
	if d.DerogadoPor != "" {
		out = append(out, Alerta{"derogado", id, fmt.Sprintf(
			"%s fue derogado / dejado sin efecto por %s. No usar como norma vigente.",
			d.Label(), r.docs[d.DerogadoPor].Label())})
	}
	if len(d.ModificadoPor) > 0 {
		var ls []string
		for _, m := range d.ModificadoPor {
			ls = append(ls, r.docs[m].Label())
		}
		out = append(out, Alerta{"modificado", id, fmt.Sprintf(
			"%s tiene modificaciones posteriores: %s. Verificar que el texto citado siga vigente.",
			d.Label(), strings.Join(ls, "; "))})
	}
	return out
}

type Historial struct {
	Doc            *Doc     `json:"doc"`
	Estado         string   `json:"estado"`
	Reemplaza      []string `json:"reemplaza"`       // documentos que este derogó
	ReemplazadoPor []string `json:"reemplazado_por"` // cadena hacia adelante
	ModificadoPor  []string `json:"modificado_por"`
	Modifica       []string `json:"modifica"`
}

func (r *Registry) Historial(id string) (*Historial, bool) {
	d, ok := r.docs[id]
	if !ok {
		return nil, false
	}
	h := &Historial{Doc: d, Estado: r.Estado(id), ModificadoPor: d.ModificadoPor}
	for _, o := range r.orden {
		if o.DerogadoPor == id {
			h.Reemplaza = append(h.Reemplaza, o.ID)
		}
		for _, m := range o.ModificadoPor {
			if m == id {
				h.Modifica = append(h.Modifica, o.ID)
			}
		}
	}
	for cur := d; cur.DerogadoPor != "" && len(h.ReemplazadoPor) < 20; cur = r.docs[cur.DerogadoPor] {
		h.ReemplazadoPor = append(h.ReemplazadoPor, cur.DerogadoPor)
	}
	return h, true
}

var jerarquia = map[string]int{"norma_sbs": 4, "basilea": 3, "politica_interna": 2, "procedimiento_interno": 1}

// Prevalece decide SIN LLM qué documento manda ante una contradicción:
// el LLM detecta el conflicto, el código decide quién gana.
func (r *Registry) Prevalece(a, b string) (ganador, criterio string) {
	da, db := r.docs[a], r.docs[b]
	if da == nil || db == nil {
		return "", "indeterminado"
	}
	derA, derB := r.Estado(a) == "derogado", r.Estado(b) == "derogado"
	if derA != derB {
		if derA {
			return b, "el otro documento está derogado"
		}
		return a, "el otro documento está derogado"
	}
	if ja, jb := jerarquia[da.Tipo], jerarquia[db.Tipo]; ja != jb {
		if ja > jb {
			return a, "mayor jerarquía normativa"
		}
		return b, "mayor jerarquía normativa"
	}
	if da.Fecha != "" && db.Fecha != "" && da.Fecha != db.Fecha {
		if da.Fecha > db.Fecha {
			return a, "norma posterior del mismo nivel"
		}
		return b, "norma posterior del mismo nivel"
	}
	return "", "indeterminado: requiere revisión humana"
}

// Detecta "877-2020", "N° 00877-2020", "G-139-2009", "circular 139-2009"...
var reNumero = regexp.MustCompile(`(?i)(?:\b([a-z]{1,2})\s?-\s?)?\b0*(\d{1,5})\s?-\s?(\d{4})\b`)

func clavesDesdeMatch(m []string) (completo, pelado string) {
	pelado = m[2] + "-" + m[3]
	if m[1] != "" {
		return strings.ToUpper(m[1]) + "-" + pelado, pelado
	}
	return pelado, pelado
}

func clavesNumero(s string) (completo, pelado string) {
	m := reNumero.FindStringSubmatch(s)
	if m == nil {
		return "", ""
	}
	return clavesDesdeMatch(m)
}

// DocIDsDesdeTexto devuelve los documentos mencionados por número en la pregunta.
func (r *Registry) DocIDsDesdeTexto(q string) []string {
	var out []string
	visto := map[string]bool{}
	for _, m := range reNumero.FindAllStringSubmatch(q, -1) {
		completo, pelado := clavesDesdeMatch(m)
		id, ok := r.byNumero[completo]
		if !ok {
			id, ok = r.byNumero[pelado]
		}
		if ok && !visto[id] {
			visto[id] = true
			out = append(out, id)
		}
	}
	return out
}
