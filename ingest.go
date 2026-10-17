package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Chunk struct {
	ChunkID string `json:"chunk_id"`
	DocID   string `json:"doc_id"`
	Ruta    string `json:"ruta"`
	Seccion string `json:"seccion,omitempty"` // TÍTULO / CAPÍTULO que lo contiene
	Pagina  int    `json:"pagina"`
	Texto   string `json:"texto"`
}

const maxChunkChars = 3000 // ~750 tokens

// ordinal: "Primero", "Décimo", "Único"... Es una lista cerrada para que
// encabezados de tabla como "Artículo PGR-001" no se tomen por artículos.
const ordinal = `(?:primer[oa]?|segund[oa]|tercer[oa]?|cuart[oa]|quint[oa]|sext[oa]|s[ée]ptim[oa]|octav[oa]|noven[oa]|` +
	`d[ée]cim[oa]|und[ée]cim[oa]|duod[ée]cim[oa]|vig[ée]sim[oa]|[úu]nic[oa])`

var (
	// "Artículo 5.-", "Artículo 5°.-", "Artículo Primero.-", "Artículo Décimo Segundo.-",
	// "ARTÍCULO 4.-", y "## Artículo 3.-" en los .md sintéticos. Exige el guion para
	// no cortar en referencias del tipo "conforme al artículo 5 del Reglamento".
	reArticulo = regexp.MustCompile(`(?im)^[ \t]*(?:#+[ \t]*)?art[íi]culo[ \t]+([0-9]+[°º]?|` + ordinal + `(?:[ \t]+` + ordinal + `)?)[ \t]*\.?[ \t]*[-–—]`)
	// Encabezados que también cortan chunks (ver limites).
	reAnexo    = regexp.MustCompile(`(?m)^[ \t]*ANEXO[ \t]+([A-Z0-9]+)[ \t]*[-–—].*$`)
	reTitulo   = regexp.MustCompile(`(?m)^[ \t]*(?:T[ÍI]TULO|CAP[ÍI]TULO)[ \t]+[IVXLC0-9]+[ \t]*[-–—].*$`)
	reMarcador = regexp.MustCompile(`(?m)^[ \t]*(CONSIDERANDO|SE RESUELVE|DISPOSICI[ÓO]N(?:ES)? (?:FINAL|COMPLEMENTARIA|TRANSITORIA)(?:E?S)?|Control de cambios)[ \t:]*$`)
	// "regla-\nmento" → "reglamento"
	reGuion = regexp.MustCompile(`(\p{L})-\n[ \t]*(\p{Ll})`)
)

type tipoLimite int

const (
	limChunk    tipoLimite = iota // abre un chunk con esta ruta
	limSeccion                    // TÍTULO/CAPÍTULO: cambia la sección, no genera chunk
	limDescarte                   // "SE RESUELVE", "Control de cambios": lo que sigue no se indexa
)

type limite struct {
	ini, finCab int // inicio del encabezado y fin de su línea
	tipo        tipoLimite
	ruta        string
	conCab      bool // el chunk incluye la línea del encabezado
}

// limites ubica todos los cortes del texto, en orden.
func limites(text string) []limite {
	var ls []limite
	for _, l := range reArticulo.FindAllStringSubmatchIndex(text, -1) {
		ls = append(ls, limite{l[0], l[1], limChunk, "Artículo " + nombreArticulo(text[l[2]:l[3]]), true})
	}
	for _, l := range reAnexo.FindAllStringSubmatchIndex(text, -1) {
		ls = append(ls, limite{l[0], l[1], limChunk, "Anexo " + text[l[2]:l[3]], true})
	}
	for _, l := range reTitulo.FindAllStringIndex(text, -1) {
		ls = append(ls, limite{l[0], l[1], limSeccion, oracion(strings.TrimSpace(text[l[0]:l[1]])), false})
	}
	for _, l := range reMarcador.FindAllStringSubmatchIndex(text, -1) {
		switch m := text[l[2]:l[3]]; {
		case m == "CONSIDERANDO":
			ls = append(ls, limite{l[0], l[1], limChunk, "Considerando", false})
		case strings.HasPrefix(m, "DISPOSICI"):
			ls = append(ls, limite{l[0], l[1], limChunk, oracion(m), false})
		default:
			ls = append(ls, limite{l[0], l[1], limDescarte, "", false})
		}
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].ini < ls[j].ini })
	return ls
}

func leerPaginas(path string) ([]string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt":
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return strings.Split(string(b), "\f"), nil
	case ".pdf":
		return pdftotext(path, false)
	}
	return nil, fmt.Errorf("formato no soportado: %s", path)
}

func chunkear(docID string, pages []string) []Chunk {
	var sb strings.Builder
	starts := make([]int, len(pages))
	for i, p := range pages {
		starts[i] = sb.Len()
		sb.WriteString(reGuion.ReplaceAllString(p, "$1$2"))
		sb.WriteString("\n")
	}
	text := sb.String()
	pagina := func(off int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > off })
	}

	type tramo struct {
		ruta, seccion string
		ini, fin      int
	}
	var tramos []tramo
	ls := limites(text)
	if len(ls) == 0 {
		if strings.TrimSpace(text) != "" {
			tramos = append(tramos, tramo{"Documento", "", 0, len(text)})
		}
	} else {
		if pre := strings.TrimSpace(text[:ls[0].ini]); len(pre) > 200 {
			tramos = append(tramos, tramo{"Preámbulo", "", 0, ls[0].ini})
		}
		seccion := ""
		for i, l := range ls {
			fin := len(text)
			if i+1 < len(ls) {
				fin = ls[i+1].ini
			}
			switch l.tipo {
			case limSeccion:
				seccion = l.ruta
			case limChunk:
				if strings.HasPrefix(l.ruta, "Anexo") {
					seccion = ""
				}
				ini := l.finCab
				if l.conCab {
					ini = l.ini
				}
				tramos = append(tramos, tramo{l.ruta, seccion, ini, fin})
			}
		}
	}

	var chunks []Chunk
	usados := map[string]int{}
	for _, t := range tramos {
		partes := partir(text, t.ini, t.fin)
		for j, p := range partes {
			ruta := t.ruta
			if len(partes) > 1 {
				ruta = fmt.Sprintf("%s (parte %d)", t.ruta, j+1)
			}
			texto := strings.Join(strings.Fields(text[p[0]:p[1]]), " ")
			if texto == "" {
				continue
			}
			id := docID + "#" + slug(ruta)
			usados[id]++
			if n := usados[id]; n > 1 { // ej: un reglamento anexo con su propio "Artículo 1"
				id = fmt.Sprintf("%s-%d", id, n)
			}
			chunks = append(chunks, Chunk{ChunkID: id, DocID: docID, Ruta: ruta, Seccion: t.seccion, Pagina: pagina(p[0]), Texto: texto})
		}
	}
	return chunks
}

// partir corta tramos largos en fin de oración / línea, sin romper runas UTF-8.
func partir(text string, ini, fin int) [][2]int {
	var out [][2]int
	for fin-ini > maxChunkChars {
		desde, limite := ini+maxChunkChars/2, ini+maxChunkChars
		ventana := text[desde:limite]
		corte := limite
		for _, sep := range []string{".\n", ". ", "\n"} {
			if i := strings.LastIndex(ventana, sep); i >= 0 {
				corte = desde + i + len(sep)
				break
			}
		}
		for corte > ini && corte < len(text) && !utf8.RuneStart(text[corte]) {
			corte--
		}
		out = append(out, [2]int{ini, corte})
		ini = corte
	}
	return append(out, [2]int{ini, fin})
}

func nombreArticulo(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "°º"))
	if s != "" && unicode.IsDigit(rune(s[0])) {
		return s
	}
	palabras := strings.Fields(strings.ToLower(s))
	for i, p := range palabras {
		r, n := utf8.DecodeRuneInString(p)
		palabras[i] = string(unicode.ToUpper(r)) + p[n:]
	}
	return strings.Join(palabras, " ")
}

func slug(s string) string {
	var b strings.Builder
	guion := false
	for _, r := range sinAcentos(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			guion = false
		} else if !guion && b.Len() > 0 {
			b.WriteByte('-')
			guion = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// conEncabezado es el "contextual chunk header": se embebe y se rerankea con
// el nombre del documento y el artículo adelante, pero se guarda sin él.
func conEncabezado(reg *Registry, c Chunk) string {
	cab := c.DocID
	if d, ok := reg.Get(c.DocID); ok {
		cab = d.Label()
	}
	if c.Seccion != "" {
		cab += " — " + c.Seccion
	}
	return cab + " — " + c.Ruta + "\n" + c.Texto
}

func idPunto(chunkID string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(chunkID))
	return h.Sum64()
}

// idDoc: el doc_id es el código en minúsculas ("PRC-001" → "prc-001").
func idDoc(codigo string) string { return strings.ToLower(strings.TrimSpace(codigo)) }

// chunksDeArchivo chunkea un archivo del corpus. Un PDF puede traer varios
// documentos (principal + anexos con su propio bloque de metadatos); un .md es
// un solo documento, el de docID.
func chunksDeArchivo(path, docID string) (map[string][]Chunk, []string, error) {
	por := map[string][]Chunk{}
	if strings.ToLower(filepath.Ext(path)) != ".pdf" {
		pages, err := leerPaginas(path)
		if err != nil {
			return nil, nil, err
		}
		por[docID] = chunkear(docID, pages)
		return por, []string{docID}, nil
	}
	a, err := extraerPDF(path)
	if err != nil {
		return nil, nil, err
	}
	var orden []string
	for _, d := range a.Docs {
		id := idDoc(d.Codigo)
		por[id] = chunkear(id, d.Paginas)
		orden = append(orden, id)
	}
	return por, orden, nil
}

func runIngest(ctx context.Context, cfg Config, reg *Registry, reset, dry bool) error {
	base := filepath.Dir(cfg.RegistryPath)
	var chunks []Chunk
	vistos := map[string]bool{}
	for _, d := range reg.Docs() {
		if d.Archivo == "" {
			log.Printf("· %s: sin archivo, se saltea", d.ID)
			continue
		}
		if vistos[d.Archivo] {
			continue // ya procesado junto con su documento padre
		}
		vistos[d.Archivo] = true
		path := d.Archivo
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		if _, err := os.Stat(path); err != nil {
			log.Printf("! %-28s no encuentro %s, se saltea", d.ID, path)
			continue
		}
		por, orden, err := chunksDeArchivo(path, d.ID)
		if err != nil {
			return err
		}
		for _, id := range orden {
			if _, ok := reg.Get(id); !ok {
				log.Printf("! %-28s está en %s pero no en el registro (corré `registro`), se saltea", id, d.Archivo)
				continue
			}
			log.Printf("· %-28s → %3d chunks", id, len(por[id]))
			chunks = append(chunks, por[id]...)
		}
	}
	if err := escribirJSONL("chunks.jsonl", chunks); err != nil {
		return err
	}
	log.Printf("total: %d chunks (revisalos en chunks.jsonl)", len(chunks))
	if dry || len(chunks) == 0 {
		return nil
	}

	emb, err := nuevoEmbedder(cfg)
	if err != nil {
		return err
	}
	qd, err := nuevoQdrant(cfg)
	if err != nil {
		return err
	}
	textos := make([]string, len(chunks))
	toks := make([][]string, len(chunks))
	total := 0
	for i, c := range chunks {
		textos[i] = conEncabezado(reg, c)
		toks[i] = tokenizar(textos[i])
		total += len(toks[i])
	}
	avgdl := float64(total) / float64(len(chunks))

	vecs, err := emb.EmbedDocs(ctx, textos)
	if err != nil {
		return err
	}
	if len(vecs) != len(chunks) || len(vecs[0]) == 0 {
		return fmt.Errorf("el embedder devolvió %d vectores para %d chunks", len(vecs), len(chunks))
	}
	if err := qd.Asegurar(ctx, len(vecs[0]), reset); err != nil {
		return fmt.Errorf("preparar colección: %w", err)
	}
	pts := make([]Punto, len(chunks))
	for i, c := range chunks {
		pts[i] = Punto{
			ID:      idPunto(c.ChunkID),
			Vector:  map[string]any{"dense": vecs[i], "bm25": bm25Doc(toks[i], avgdl)},
			Payload: c,
		}
	}
	if err := qd.Upsert(ctx, pts); err != nil {
		return err
	}
	log.Printf("indexados %d chunks en %q ✓", len(pts), cfg.Coleccion)
	return nil
}

func escribirJSONL(path string, chunks []Chunk) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, c := range chunks {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return w.Flush()
}
