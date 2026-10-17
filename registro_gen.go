package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// ---------- `registro`: genera registry.json desde los PDFs del corpus ----------
//
// Fuente: el bloque de metadatos de cada PDF ("Reemplaza a", "Relación con otros
// documentos", vigencias) y sus tablas de correspondencia. Lo que las reglas no
// entiendan se corrige en registry.overrides.json, que se aplica encima.

var (
	reRelModifica  = regexp.MustCompile(`(?i)\bmodifica(?:\s+a)?\s+(` + reCodigo.String() + `)`)
	reRelPrevalece = regexp.MustCompile(`(?i)\bprevalece\b[^.]*?\bsobre\s+(` + reCodigo.String() + `)`)
	reRelTermino   = regexp.MustCompile(`(?i)interpretaci[óo]n\s+del\s+t[ée]rmino\s+["“«]([^"”»]+)["”»]\s+en\s+`)
	reRelSinEfecto = regexp.MustCompile(`(?i)\b(?:deja|dejan|queda|quedan)\s+sin\s+efecto\s+`)
	// Relaciones que no generan nada en el registro pero se reconocen (para no avisar).
	reRelInformativa = regexp.MustCompile(`(?i)caduca|escisi[óo]n|se divide|no tiene documento de reemplazo`)
)

// tramo devuelve s[i:] hasta el fin de la oración (un ". " seguido de mayúscula
// que no sea el "Art. " de una cita de artículo).
func tramo(s string, i int) string {
	t := s[i:]
	for j := 0; j+2 < len(t); j++ {
		if t[j] != '.' || t[j+1] != ' ' {
			continue
		}
		if antes := t[:j]; strings.HasSuffix(antes, "Art") || strings.HasSuffix(antes, "Arts") || strings.HasSuffix(antes, "N°") {
			continue
		}
		if r := []rune(t[j+2:]); len(r) > 0 && unicode.IsUpper(r[0]) {
			return t[:j]
		}
	}
	return t
}

func codigos(s string) []string {
	var out []string
	for _, c := range reCodigo.FindAllString(s, -1) {
		if id := idDoc(c); !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func fechaISO(s string) string {
	if t, ok := parseFecha(s); ok {
		return isoFecha(t)
	}
	return ""
}

func agregar(xs []string, ys ...string) []string {
	for _, y := range ys {
		if !slices.Contains(xs, y) {
			xs = append(xs, y)
		}
	}
	return xs
}

// docDesdeExtraido arma el Doc con las relaciones que se leen de su metadata.
// Devuelve avisos para lo que no se pudo interpretar.
func docDesdeExtraido(d DocExtraido, archivo string, chunks []Chunk) (*Doc, []string) {
	m := d.Meta
	id := idDoc(d.Codigo)
	doc := &Doc{ID: id, Numero: d.Codigo, Titulo: d.Titulo, Tipo: m["tipo de documento"], Archivo: archivo,
		Padre: idDoc(d.Padre), Fecha: fechaISO(m["fecha de aprobacion"]),
		VigenteDesde: fechaISO(m["vigente desde"]), VigenteHasta: fechaISO(m["vigente hasta"])}
	if doc.Tipo == "" && d.Padre != "" {
		doc.Tipo = "Anexo de " + d.Padre
	}
	var avisos []string
	if doc.VigenteDesde == "" && d.Padre == "" {
		avisos = append(avisos, id+": sin «Vigente desde» legible")
	}
	if h := m["vigente hasta"]; h != "" && doc.VigenteHasta == "" && !strings.HasPrefix(sinAcentos(h), "indefinid") {
		avisos = append(avisos, fmt.Sprintf("%s: «Vigente hasta» = %q no es una fecha", id, h))
	}

	if rem := m["reemplaza a"]; rem != "" && !strings.HasPrefix(sinAcentos(rem), "ningun") {
		doc.Reemplaza = agregar(doc.Reemplaza, codigos(rem)...)
	}
	rel := m["relacion con otros documentos"]
	reconocida := rel == "" || reRelInformativa.MatchString(rel)
	for _, x := range reRelSinEfecto.FindAllStringIndex(rel, -1) {
		doc.Reemplaza = agregar(doc.Reemplaza, codigos(tramo(rel, x[1]))...)
		reconocida = true
	}
	for _, x := range reRelModifica.FindAllStringSubmatchIndex(rel, -1) {
		doc.Modifica = append(doc.Modifica, RelArt{idDoc(rel[x[2]:x[3]]), numerosArt(tramo(rel, x[3]))})
		reconocida = true
	}
	for _, x := range reRelPrevalece.FindAllStringSubmatchIndex(rel, -1) {
		doc.PrevaleceSobre = append(doc.PrevaleceSobre, RelArt{idDoc(rel[x[2]:x[3]]), numerosArt(tramo(rel, x[3]))})
		reconocida = true
	}
	for _, x := range reRelTermino.FindAllStringSubmatchIndex(rel, -1) {
		termino := rel[x[2]:x[3]]
		rd := Redefinicion{Termino: termino, Chunk: chunkDefinicion(chunks, termino), Docs: codigos(tramo(rel, x[1]))}
		if rd.Chunk == "" {
			avisos = append(avisos, fmt.Sprintf("%s: redefine «%s» pero no encontré el artículo con la definición", id, termino))
			continue
		}
		doc.Redefine = append(doc.Redefine, rd)
		reconocida = true
	}
	doc.Reemplaza = slices.DeleteFunc(doc.Reemplaza, func(x string) bool { return x == id })
	if !reconocida {
		avisos = append(avisos, fmt.Sprintf("%s: no entendí la relación %q (cargala en registry.overrides.json)", id, rel))
	}
	return doc, avisos
}

// chunkDefinicion: el primer artículo que dice "Se considera <término>"; si no
// hay, el primero cuya ruta o texto hable de definición y lo nombre.
func chunkDefinicion(chunks []Chunk, termino string) string {
	t := normalizar(termino)
	for _, c := range chunks {
		if strings.Contains(normalizar(c.Texto), "se considera "+t) {
			return c.ChunkID
		}
	}
	for _, c := range chunks {
		if n := normalizar(c.Texto); strings.Contains(n, t) && strings.Contains(n, "definici") {
			return c.ChunkID
		}
	}
	return ""
}

// generarRegistro lee corpusDir/*.pdf y devuelve los documentos (en orden de
// archivo; los derivados después de su padre) y los avisos para revisar.
func generarRegistro(corpusDir, baseRegistro string) ([]*Doc, []string, error) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.pdf"))
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(files)
	var docs []*Doc
	var avisos []string
	todos := map[string]bool{} // chunk_ids generados, para validar correspondencias
	type pendiente struct {
		doc  *Doc
		fila FilaCorresp
	}
	var filas []pendiente
	for _, f := range files {
		a, err := extraerPDF(f)
		if err != nil {
			return nil, nil, err
		}
		avisos = append(avisos, a.Avisos...)
		archivo, err := filepath.Rel(baseRegistro, f)
		if err != nil {
			archivo = f
		}
		var principal *Doc
		for _, d := range a.Docs {
			cs := chunkear(idDoc(d.Codigo), d.Paginas)
			for _, c := range cs {
				todos[c.ChunkID] = true
			}
			doc, av := docDesdeExtraido(d, filepath.ToSlash(archivo), cs)
			avisos = append(avisos, av...)
			docs = append(docs, doc)
			if principal == nil {
				principal = doc
			}
		}
		for _, fila := range a.Filas {
			filas = append(filas, pendiente{principal, fila})
		}
	}
	// Correspondencias de artículos (tablas de distribución / consolidación).
	for _, p := range filas {
		orig := idDoc(p.fila.OrigenDoc)
		if orig == "" {
			if len(p.doc.Reemplaza) != 1 {
				avisos = append(avisos, fmt.Sprintf("%s: fila de correspondencia sin documento de origen", p.doc.ID))
				continue
			}
			orig = p.doc.Reemplaza[0]
		}
		dst := p.doc.ID
		if p.fila.DestinoDoc != "" {
			dst = idDoc(p.fila.DestinoDoc)
		}
		destino := fmt.Sprintf("%s#articulo-%d", dst, p.fila.DestinoArt)
		for _, art := range p.fila.OrigenArts {
			origen := fmt.Sprintf("%s#articulo-%d", orig, art)
			if !todos[origen] || !todos[destino] {
				avisos = append(avisos, fmt.Sprintf("%s: correspondencia %s → %s apunta a un chunk que no existe", p.doc.ID, origen, destino))
				continue
			}
			if p.doc.Correspondencia == nil {
				p.doc.Correspondencia = map[string]string{}
			}
			p.doc.Correspondencia[origen] = destino
		}
	}
	return docs, avisos, nil
}

// aplicarOverrides: cada entrada de overrides es un objeto parcial con doc_id.
// Sus campos pisan a los generados; "excluir": true lo saca; un doc_id nuevo se agrega.
func aplicarOverrides(docs []*Doc, path string) ([]*Doc, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return docs, nil
	}
	if err != nil {
		return nil, err
	}
	var ovs []map[string]any
	if err := json.Unmarshal(data, &ovs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, ov := range ovs {
		id, _ := ov["doc_id"].(string)
		if id == "" {
			return nil, fmt.Errorf("%s: override sin doc_id", path)
		}
		i := slices.IndexFunc(docs, func(d *Doc) bool { return d.ID == id })
		if excluir, _ := ov["excluir"].(bool); excluir {
			if i >= 0 {
				docs = slices.Delete(docs, i, i+1)
			}
			continue
		}
		delete(ov, "excluir")
		base := map[string]any{}
		if i >= 0 {
			b, _ := json.Marshal(docs[i])
			json.Unmarshal(b, &base)
		}
		for k, v := range ov {
			base[k] = v
		}
		b, _ := json.Marshal(base)
		d := &Doc{}
		if err := json.Unmarshal(b, d); err != nil {
			return nil, fmt.Errorf("%s (%s): %w", path, id, err)
		}
		if i >= 0 {
			docs[i] = d
		} else {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

func marshalRegistro(docs []*Doc) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(docs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// diffRegistro imprime qué documentos se agregan, quitan o cambian (y qué campos).
func diffRegistro(viejo []byte, nuevos []*Doc) []string {
	var antes []*Doc
	json.Unmarshal(viejo, &antes)
	porID := map[string]*Doc{}
	for _, d := range antes {
		porID[d.ID] = d
	}
	var out []string
	for _, d := range nuevos {
		v, ok := porID[d.ID]
		delete(porID, d.ID)
		if !ok {
			out = append(out, "+ "+d.ID)
			continue
		}
		var campos []string
		rv, rn := reflect.ValueOf(*v), reflect.ValueOf(*d)
		for i := 0; i < rv.NumField(); i++ {
			if !reflect.DeepEqual(rv.Field(i).Interface(), rn.Field(i).Interface()) {
				campos = append(campos, strings.Split(rv.Type().Field(i).Tag.Get("json"), ",")[0])
			}
		}
		if len(campos) > 0 {
			out = append(out, "~ "+d.ID+" ("+strings.Join(campos, ", ")+")")
		}
	}
	for _, d := range antes {
		if _, quedo := porID[d.ID]; quedo {
			out = append(out, "- "+d.ID)
		}
	}
	return out
}

func runRegistro(corpusDir, overrides, salida string, dry bool) error {
	docs, avisos, err := generarRegistro(corpusDir, filepath.Dir(salida))
	if err != nil {
		return err
	}
	if docs, err = aplicarOverrides(docs, overrides); err != nil {
		return err
	}
	if _, err := nuevoRegistry(docs); err != nil {
		return fmt.Errorf("el registro generado no es válido: %w", err)
	}
	for _, d := range docs {
		var rel []string
		if len(d.Reemplaza) > 0 {
			rel = append(rel, "reemplaza "+strings.Join(d.Reemplaza, ", "))
		}
		for _, m := range d.Modifica {
			rel = append(rel, "modifica "+m.Doc+artsTxt(m.Articulos))
		}
		for _, p := range d.PrevaleceSobre {
			rel = append(rel, "prevalece sobre "+p.Doc+artsTxt(p.Articulos))
		}
		for _, rd := range d.Redefine {
			rel = append(rel, fmt.Sprintf("redefine «%s» (%s)", rd.Termino, rd.Chunk))
		}
		if n := len(d.Correspondencia); n > 0 {
			rel = append(rel, fmt.Sprintf("%d correspondencias", n))
		}
		hasta := d.VigenteHasta
		if hasta == "" {
			hasta = "indef."
		}
		log.Printf("· %-18s %s → %-10s %s", d.ID, d.VigenteDesde, hasta, strings.Join(rel, "; "))
	}
	for _, a := range avisos {
		log.Printf("! %s", a)
	}
	out, err := marshalRegistro(docs)
	if err != nil {
		return err
	}
	viejo, _ := os.ReadFile(salida)
	cambios := diffRegistro(viejo, docs)
	for _, c := range cambios {
		log.Printf("  %s", c)
	}
	if dry {
		log.Printf("-dry: no escribo %s (%d documentos, %d cambios)", salida, len(docs), len(cambios))
		return nil
	}
	if err := os.WriteFile(salida, out, 0o644); err != nil {
		return err
	}
	log.Printf("escribí %s: %d documentos, %d cambios. Revisá el diff antes de commitear.", salida, len(docs), len(cambios))
	return nil
}
