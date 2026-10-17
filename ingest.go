package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
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
	Pagina  int    `json:"pagina"`
	Texto   string `json:"texto"`
}

const maxChunkChars = 3000 // ~750 tokens

var (
	// "Artículo 5.-", "Artículo 5°.-", "Artículo Primero.-", "Artículo Décimo Segundo.-",
	// "ARTÍCULO 4.-", y "## Artículo 3.-" en los .md sintéticos. Exige el guion para
	// no cortar en referencias del tipo "conforme al artículo 5 del Reglamento".
	reArticulo = regexp.MustCompile(`(?im)^[ \t]*(?:#+[ \t]*)?art[íi]culo[ \t]+([0-9]+[°º]?|[a-záéíóúñ]+(?:[ \t]+[a-záéíóúñ]+)?)[ \t]*\.?[ \t]*[-–—]`)
	// "regla-\nmento" → "reglamento"
	reGuion = regexp.MustCompile(`(\p{L})-\n[ \t]*(\p{Ll})`)
)

func leerPaginas(path string) ([]string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt":
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return strings.Split(string(b), "\f"), nil
	case ".pdf":
		// Sin -layout: El Peruano publica a varias columnas y el modo por defecto
		// respeta mejor el orden de lectura. pdftotext separa páginas con \f.
		out, err := exec.Command("pdftotext", "-enc", "UTF-8", path, "-").Output()
		if err != nil {
			return nil, fmt.Errorf("pdftotext %s: %w (¿está instalado poppler-utils?)", path, err)
		}
		pages := strings.Split(string(out), "\f")
		if n := len(pages); n > 1 && strings.TrimSpace(pages[n-1]) == "" {
			pages = pages[:n-1]
		}
		return pages, nil
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
		ruta     string
		ini, fin int
	}
	var tramos []tramo
	locs := reArticulo.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		tramos = append(tramos, tramo{"Documento", 0, len(text)})
	} else {
		if pre := strings.TrimSpace(text[:locs[0][0]]); len(pre) > 200 {
			tramos = append(tramos, tramo{"Preámbulo", 0, locs[0][0]})
		}
		for i, l := range locs {
			fin := len(text)
			if i+1 < len(locs) {
				fin = locs[i+1][0]
			}
			tramos = append(tramos, tramo{"Artículo " + nombreArticulo(text[l[2]:l[3]]), l[0], fin})
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
			chunks = append(chunks, Chunk{ChunkID: id, DocID: docID, Ruta: ruta, Pagina: pagina(p[0]), Texto: texto})
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
	return cab + " — " + c.Ruta + "\n" + c.Texto
}

func idPunto(chunkID string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(chunkID))
	return h.Sum64()
}

func runIngest(ctx context.Context, cfg Config, reg *Registry, reset, dry bool) error {
	base := filepath.Dir(cfg.RegistryPath)
	var chunks []Chunk
	for _, d := range reg.Docs() {
		if d.Archivo == "" {
			log.Printf("· %s: sin archivo, se saltea", d.ID)
			continue
		}
		path := d.Archivo
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		if _, err := os.Stat(path); err != nil {
			log.Printf("! %-28s no encuentro %s, se saltea", d.ID, path)
			continue
		}
		pages, err := leerPaginas(path)
		if err != nil {
			return err
		}
		cs := chunkear(d.ID, pages)
		log.Printf("· %-28s %3d págs → %3d chunks", d.ID, len(pages), len(cs))
		chunks = append(chunks, cs...)
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
