package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// ---------- Extracción de PDFs con plantilla "bloque de metadatos + artículos" ----------
//
// Cada PDF trae un bloque de metadatos (Código, Vigente desde/hasta, Reemplaza a,
// Relación con otros documentos...), el cuerpo por artículos, anexos y el control
// de cambios. Un "ANEXO A - ... (PRC-001)" seguido de su propio bloque de
// metadatos es un documento derivado dentro del mismo archivo.
//
// Dos pasadas de pdftotext: la normal (orden de lectura) para el texto y los
// metadatos, y -layout solo para las tablas de correspondencia de artículos,
// donde cada fila queda en una línea.

type DocExtraido struct {
	Codigo, Titulo string
	Padre          string            // código del documento que lo contiene ("" si es el principal)
	Meta           map[string]string // clave normalizada (ver clavesMeta) → valor
	Paginas        []string          // cuerpo limpio; las páginas fuera del documento quedan vacías
}

// FilaCorresp es una fila de una tabla "artículo de origen → artículo de destino".
// OrigenDoc "" = el documento que este reemplaza; DestinoDoc "" = el principal del archivo.
type FilaCorresp struct {
	OrigenDoc  string
	OrigenArts []int
	DestinoDoc string
	DestinoArt int
}

type ArchivoPDF struct {
	Docs   []DocExtraido // el principal primero
	Filas  []FilaCorresp
	Avisos []string
}

var (
	reCodigo       = regexp.MustCompile(`\b[A-Z]{2,4}(?:-[A-Z]{2,4})?-\d{3}(?:-\d{2,4})?\b`)
	reCodigoExacto = regexp.MustCompile(`^` + reCodigo.String() + `$`)
	reAnexoDoc     = regexp.MustCompile(`^ANEXO\s+[A-Z0-9]+\s*[-–—]\s*(.+?)\s*\((` + reCodigo.String() + `)\)\s*$`)
	rePiePagina    = regexp.MustCompile(`\|\s*P[áa]gina\s+\d+\s+de\s+\d+\s*$`)
	reFirma        = regexp.MustCompile(`^_{5,}$`)
	reInicioCuerpo = regexp.MustCompile(`(?i)^(?:T[ÍI]TULO|CAP[ÍI]TULO)\s+[IVXLC0-9]+\b|^(?:CONSIDERANDO|SE RESUELVE)\s*:?$|^ANEXO\s+[A-Z0-9]+\s*[-–—]|` +
		`^art[íi]culo\s+(?:\d+|` + ordinal + `)`)
	reArtNums = regexp.MustCompile(`(\d+)\s*°`)
)

// clavesMeta: etiquetas del bloque de metadatos, normalizadas con claveMeta.
var clavesMeta = map[string]bool{"codigo": true, "version": true, "tipo de documento": true, "area responsable": true,
	"aprobado por": true, "fecha de aprobacion": true, "vigente desde": true, "vigente hasta": true, "estado": true,
	"reemplaza a": true, "relacion con otros documentos": true, "dominios": true, "dominio": true}

var reParentesis = regexp.MustCompile(`\([^)]*\)`)

func claveMeta(s string) string {
	s = reParentesis.ReplaceAllString(sinAcentos(s), "")
	return strings.Join(strings.Fields(s), " ")
}

func pdftotext(path string, layout bool) ([]string, error) {
	args := []string{"-enc", "UTF-8"}
	if layout {
		args = append(args, "-layout")
	}
	out, err := exec.Command("pdftotext", append(args, path, "-")...).Output()
	if err != nil {
		return nil, fmt.Errorf("pdftotext %s: %w (¿está instalado poppler-utils?)", path, err)
	}
	pages := strings.Split(string(out), "\f")
	if n := len(pages); n > 1 && strings.TrimSpace(pages[n-1]) == "" {
		pages = pages[:n-1]
	}
	return pages, nil
}

func extraerPDF(path string) (*ArchivoPDF, error) {
	raw, err := pdftotext(path, false)
	if err != nil {
		return nil, err
	}
	layout, err := pdftotext(path, true)
	if err != nil {
		return nil, err
	}
	a := extraerDeTexto(raw, codigoDesdeArchivo(path), ruidoDeCarpeta(filepath.Dir(path)))
	a.Filas = parsearCorrespondencias(layout)
	return a, nil
}

// codigoDesdeArchivo: "06_RD-061-2025_Escision.pdf" → "RD-061-2025" (respaldo si no hay bloque de metadatos).
func codigoDesdeArchivo(path string) string {
	if m := reCodigo.FindString(filepath.Base(path)); m != "" {
		return m
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

type linea struct {
	pag int
	s   string // TrimSpace; "" = línea en blanco
}

// lineasRuido: líneas que aparecen en la zona de encabezado/pie de TODAS las
// páginas (nombre del banco, "Uso interno - ...", leyendas). Con una sola página
// no se puede distinguir y no se quita nada por este criterio.
func lineasRuido(pages []string) map[string]bool {
	if len(pages) < 2 {
		return nil
	}
	var comun map[string]bool
	for _, p := range pages {
		var ls []string
		for _, l := range strings.Split(p, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				ls = append(ls, l)
			}
		}
		zona := map[string]bool{}
		for i, l := range ls {
			if i < 3 || i >= len(ls)-6 {
				zona[l] = true
			}
		}
		if comun == nil {
			comun = zona
			continue
		}
		for l := range comun {
			if !zona[l] {
				delete(comun, l)
			}
		}
	}
	return comun
}

// ruidoCarpetas: por carpeta, las líneas de encabezado/pie que se repiten en al
// menos dos PDFs. Cubre un PDF de una sola página, donde lineasRuido no puede
// comparar contra otras páginas del mismo archivo.
var ruidoCarpetas sync.Map

func ruidoDeCarpeta(dir string) map[string]bool {
	if v, ok := ruidoCarpetas.Load(dir); ok {
		return v.(map[string]bool)
	}
	cuenta := map[string]int{}
	fs, _ := filepath.Glob(filepath.Join(dir, "*.pdf"))
	for _, f := range fs {
		if pages, err := pdftotext(f, false); err == nil {
			for l := range lineasRuido(pages) {
				cuenta[l]++
			}
		}
	}
	ruido := map[string]bool{}
	for l, n := range cuenta {
		if n >= 2 {
			ruido[l] = true
		}
	}
	ruidoCarpetas.Store(dir, ruido)
	return ruido
}

func limpiarLineas(pages []string, extra map[string]bool) []linea {
	ruido := lineasRuido(pages)
	var out []linea
	for i, p := range pages {
		for _, l := range strings.Split(p, "\n") {
			l = strings.TrimSpace(l)
			if reFirma.MatchString(l) {
				break // bloque de firmas: hasta el final de la página
			}
			if ruido[l] || extra[l] || rePiePagina.MatchString(l) {
				continue
			}
			out = append(out, linea{i, l})
		}
	}
	return out
}

func extraerDeTexto(pages []string, codigoArchivo string, ruidoExtra map[string]bool) *ArchivoPDF {
	ls := limpiarLineas(pages, ruidoExtra)
	// Inicios de documento: el principal en 0, y cada ANEXO con código seguido de "Código".
	inicios := []int{0}
	anexo := map[int][]string{}
	for i, l := range ls {
		m := reAnexoDoc.FindStringSubmatch(l.s)
		if m == nil {
			continue
		}
		for j, vistas := i+1, 0; j < len(ls) && vistas < 3; j++ {
			if ls[j].s == "" {
				continue
			}
			if ls[j].s == "Código" {
				inicios = append(inicios, i)
				anexo[i] = m
				break
			}
			vistas++
		}
	}
	a := &ArchivoPDF{}
	for k, ini := range inicios {
		fin := len(ls)
		if k+1 < len(inicios) {
			fin = inicios[k+1]
		}
		seg := ls[ini:fin]
		cod := -1
		for i, l := range seg {
			if l.s == "Código" {
				cod = i
				break
			}
		}
		d := DocExtraido{Meta: map[string]string{}, Paginas: make([]string, len(pages))}
		cuerpo := 0
		if cod >= 0 {
			cuerpo = len(seg)
			for i := cod + 1; i < len(seg); i++ {
				if reInicioCuerpo.MatchString(seg[i].s) {
					cuerpo = i
					break
				}
			}
			d.Meta = parsearMetadatos(seg[cod:cuerpo])
		} else {
			a.Avisos = append(a.Avisos, fmt.Sprintf("%s: sin bloque de metadatos", codigoArchivo))
		}
		if m := anexo[ini]; m != nil {
			d.Titulo, d.Padre = oracion(m[1]), a.Docs[0].Codigo
		} else if cod > 0 {
			d.Titulo = tituloDe(seg[:cod])
		}
		d.Codigo = d.Meta["codigo"]
		if d.Codigo == "" {
			if m := anexo[ini]; m != nil {
				d.Codigo = m[2]
			} else {
				d.Codigo = codigoArchivo
			}
		}
		for _, l := range seg[cuerpo:] {
			d.Paginas[l.pag] += l.s + "\n"
		}
		a.Docs = append(a.Docs, d)
	}
	return a
}

// parsearMetadatos: en modo normal pdftotext deja etiqueta y valor en párrafos
// alternados; una etiqueta puede ocupar dos líneas ("Relación con otros /
// documentos") y un valor también. Un párrafo que no es etiqueta se suma al
// valor de la última etiqueta, salvo los avisos en mayúsculas.
func parsearMetadatos(ls []linea) map[string]string {
	meta := map[string]string{}
	var parrafos []string
	var cur []string
	for i := 0; i <= len(ls); i++ { // i == len(ls) cierra el último párrafo
		if i == len(ls) || ls[i].s == "" {
			if len(cur) > 0 {
				parrafos = append(parrafos, strings.Join(cur, " "))
				cur = nil
			}
			continue
		}
		cur = append(cur, ls[i].s)
	}
	clave := ""
	for _, p := range parrafos {
		if k := claveMeta(p); clavesMeta[k] {
			clave = k
			if _, ok := meta[k]; !ok {
				meta[k] = ""
			}
			continue
		}
		if esAviso(p) && (clave == "" || meta[clave] != "") { // si la etiqueta espera valor, es su valor
			meta["aviso"] = strings.TrimSpace(meta["aviso"] + " " + p)
			continue
		}
		if clave != "" {
			meta[clave] = strings.TrimSpace(meta[clave] + " " + p)
		}
	}
	return meta
}

// esAviso: banner en mayúsculas del tipo "DOCUMENTO DE VIGENCIA TEMPORAL - CADUCA ...".
func esAviso(s string) bool {
	return len([]rune(s)) > 30 && esMayusculas(s)
}

func esMayusculas(s string) bool {
	letras := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letras++
			if unicode.IsLower(r) {
				return false
			}
		}
	}
	return letras > 0
}

// tituloDe: líneas en mayúsculas antes del bloque de metadatos (el subtítulo va en minúsculas).
func tituloDe(ls []linea) string {
	var t []string
	for _, l := range ls {
		if l.s == "" {
			continue
		}
		if !esMayusculas(l.s) {
			if len(t) > 0 {
				break
			}
			continue // "Uso interno - ..." si no se quitó como ruido
		}
		t = append(t, l.s)
	}
	return oracion(strings.Join(t, " "))
}

// oracion pasa "POLÍTICA DE RIESGO DE CRÉDITO" a "Política de riesgo de crédito",
// sin tocar códigos, siglas con "/" ni números romanos ("LA/FT", "PGR-001", "II").
func oracion(s string) string {
	ws := strings.Fields(s)
	mayus := true // primera palabra, o la que sigue a " - " o a ":"
	for i, w := range ws {
		if w == "-" || w == "–" {
			mayus = true
			continue
		}
		if !strings.ContainsAny(w, "0123456789/°") && !esRomano(w) {
			lw := []rune(strings.ToLower(w))
			if mayus {
				lw[0] = unicode.ToUpper(lw[0])
			}
			ws[i] = string(lw)
		}
		mayus = strings.HasSuffix(w, ":")
	}
	return strings.Join(ws, " ")
}

func esRomano(w string) bool {
	w = strings.TrimRight(w, ".,:")
	if w == "" {
		return false
	}
	for _, r := range w {
		if !strings.ContainsRune("IVXLC", r) {
			return false
		}
	}
	return true
}

var reCeldas = regexp.MustCompile(`\s{2,}`)

// parsearCorrespondencias lee, del texto -layout, las filas de tabla con al
// menos dos celdas "Art. N°": la primera es el artículo de origen y la última el
// de destino. Un código de documento antes de la primera es el documento de
// origen; uno entre ambas, el de destino.
func parsearCorrespondencias(layout []string) []FilaCorresp {
	var out []FilaCorresp
	for _, p := range layout {
		for _, l := range strings.Split(p, "\n") {
			celdas := reCeldas.Split(strings.TrimSpace(l), -1)
			var arts []int
			for i, c := range celdas {
				if strings.HasPrefix(c, "Art. ") || strings.HasPrefix(c, "Arts. ") {
					arts = append(arts, i)
				}
			}
			if len(arts) < 2 {
				continue
			}
			f := FilaCorresp{OrigenArts: numerosArt(celdas[arts[0]])}
			if d := numerosArt(celdas[arts[len(arts)-1]]); len(d) > 0 {
				f.DestinoArt = d[0]
			}
			for i, c := range celdas {
				if !reCodigoExacto.MatchString(c) {
					continue
				}
				if i < arts[0] {
					f.OrigenDoc = c
				} else if i < arts[len(arts)-1] {
					f.DestinoDoc = c
				}
			}
			if len(f.OrigenArts) > 0 && f.DestinoArt > 0 {
				out = append(out, f)
			}
		}
	}
	return out
}

func numerosArt(s string) []int {
	var out []int
	for _, m := range reArtNums.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}
