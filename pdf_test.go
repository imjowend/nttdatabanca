package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reSufijoDup: "doc#articulo-1-2" = el mismo artículo apareció dos veces en un documento.
var reSufijoDup = regexp.MustCompile(`#[a-z]+-\d+-\d+$`)

// Extracción real de los PDFs del corpus (necesita pdftotext; no gasta créditos).
func TestExtraerPDFsDelCorpus(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("sin pdftotext")
	}
	fs, _ := filepath.Glob("corpus/*.pdf")
	if len(fs) < 9 {
		t.Fatalf("esperaba al menos 9 PDFs en corpus/, hay %d", len(fs))
	}
	chunks := map[string]Chunk{}
	var docs []string
	for _, f := range fs {
		a, err := extraerPDF(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range a.Docs {
			id := idDoc(d.Codigo)
			docs = append(docs, id)
			for _, c := range chunkear(id, d.Paginas) {
				if _, dup := chunks[c.ChunkID]; dup {
					t.Errorf("chunk_id duplicado: %s", c.ChunkID)
				}
				chunks[c.ChunkID] = c
				for _, ruido := range []string{"Documento ficticio", "Página 1 de", "BANCO ANDINO DEL SUR", "Uso interno -",
					"Control de cambios", "Elaborado por", "Vigente desde 01", "Código PGR"} {
					if strings.Contains(c.Texto, ruido) {
						t.Errorf("%s contiene ruido %q", c.ChunkID, ruido)
					}
				}
				if reSufijoDup.MatchString(c.ChunkID) || strings.Contains(c.ChunkID, "articulo-pgr") {
					t.Errorf("chunk_id sospechoso: %s", c.ChunkID)
				}
			}
		}
	}
	for _, id := range []string{"prc-001", "prm-001", "pro-001", "rd-061-2025"} {
		if !strings.Contains(","+strings.Join(docs, ",")+",", ","+id+",") {
			t.Errorf("falta el documento %s (docs: %v)", id, docs)
		}
	}
	esperados := map[string]string{
		"prm-001#articulo-2":          "no excede el 2.5% del patrimonio efectivo",
		"prc-001#articulo-1":          "Regular la gestión del riesgo de crédito",
		"pro-001#articulo-2":          "cuarenta y ocho (48) horas",
		"man-pla-001#articulo-7":      "veinticuatro (24) horas",
		"cir-tc-021-2024#articulo-4":  "39.9%",
		"mod-prc-001-01#anexo-1":      "provisión genérica de 5.5%",
		"rex-001-2026#articulo-5":     "Gerencia de Agencia",
		"lin-car-001-2026#articulo-2": "atraso mayor a treinta (30) días",
		"rd-061-2025#considerando":    "Que la Política General",
		"pgr-001#anexo-1":             "RESUMEN DE LÍMITES Y PLAZOS",
	}
	for id, txt := range esperados {
		c, ok := chunks[id]
		if !ok {
			t.Errorf("falta el chunk %s", id)
			continue
		}
		if !strings.Contains(c.Texto, txt) {
			t.Errorf("%s no contiene %q: %q", id, txt, recortar(c.Texto, 200))
		}
	}
	if s := chunks["pgr-001#articulo-8"].Seccion; s != "Título III - Riesgo de mercado" {
		t.Errorf("sección de PGR Art. 8 = %q", s)
	}
}

func TestParsearMetadatos(t *testing.T) {
	// Formato de pdftotext en modo normal: etiqueta y valor en párrafos alternados.
	raw := "Código\n\nMAN-PLA-001\n\nVigente desde\n(valid_from)\n\n01/02/2025\n\nEstado\n\nVIGENTE\n\n" +
		"Reemplaza a\n\nCIR-PLA-007-2021;\nCIR-PLA-011-2022\n\nRelación con otros\ndocumentos\n\nDeja sin efecto ambas.\n\n" +
		"Dominios\n\nPrevención LA/FT\n\nDOCUMENTO DE VIGENCIA TEMPORAL - CADUCA EL 31/12/2024\n"
	var ls []linea
	for _, l := range strings.Split(raw, "\n") {
		ls = append(ls, linea{0, l})
	}
	m := parsearMetadatos(ls)
	want := map[string]string{"codigo": "MAN-PLA-001", "vigente desde": "01/02/2025", "reemplaza a": "CIR-PLA-007-2021; CIR-PLA-011-2022",
		"relacion con otros documentos": "Deja sin efecto ambas.", "dominios": "Prevención LA/FT",
		"aviso": "DOCUMENTO DE VIGENCIA TEMPORAL - CADUCA EL 31/12/2024"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("meta[%q] = %q, quiero %q", k, m[k], v)
		}
	}
}

func TestRelacionesDesdeMetadatos(t *testing.T) {
	chunks := []Chunk{{ChunkID: "lin#articulo-2", Texto: "Artículo 2°.- Nueva definición. Se considera Cliente de Alto Riesgo a la persona..."}}
	casos := []struct {
		meta  map[string]string
		check func(*Doc) bool
	}{
		{map[string]string{"relacion con otros documentos": "Modifica a PRC-001 (Anexo A de RD-061-2025): Art. 4° y Art. 5°. El resto de PRC-001 sigue vigente sin cambios."},
			func(d *Doc) bool {
				return len(d.Modifica) == 1 && d.Modifica[0].Doc == "prc-001" && artsTxt(d.Modifica[0].Articulos) == " Art. 4°, 5°"
			}},
		{map[string]string{"relacion con otros documentos": "Prevalece temporalmente sobre PRC-001 Art. 5° (reprogramación). Al vencer, PRC-001 Art. 5° vuelve a aplicarse."},
			func(d *Doc) bool {
				return len(d.PrevaleceSobre) == 1 && d.PrevaleceSobre[0].Doc == "prc-001" && artsTxt(d.PrevaleceSobre[0].Articulos) == " Art. 5°"
			}},
		{map[string]string{"relacion con otros documentos": `Impacto cruzado: cambia la interpretación del término "Cliente de Alto Riesgo" en PRC-001 y MAN-PLA-001, que siguen vigentes.`},
			func(d *Doc) bool {
				return len(d.Redefine) == 1 && d.Redefine[0].Chunk == "lin#articulo-2" && strings.Join(d.Redefine[0].Docs, ",") == "prc-001,man-pla-001"
			}},
		{map[string]string{"reemplaza a": "Ninguno (no sustituye a PRC-001)"}, func(d *Doc) bool { return len(d.Reemplaza) == 0 }},
		{map[string]string{"reemplaza a": "PGR-001 (Arts. 3° a 7°)"}, func(d *Doc) bool { return strings.Join(d.Reemplaza, ",") == "pgr-001" }},
		{map[string]string{"relacion con otros documentos": "Consolidación (muchos a uno): deja sin efecto CIR-PLA-007-2021 y CIR-PLA-011-2022 e incorpora su contenido."},
			func(d *Doc) bool { return strings.Join(d.Reemplaza, ",") == "cir-pla-007-2021,cir-pla-011-2022" }},
	}
	for i, c := range casos {
		c.meta["vigente desde"] = "01/01/2026"
		d, avisos := docDesdeExtraido(DocExtraido{Codigo: "LIN", Meta: c.meta}, "x.pdf", chunks)
		if !c.check(d) || len(avisos) > 0 {
			t.Errorf("caso %d: doc=%+v avisos=%v", i+1, d, avisos)
		}
	}
	_, avisos := docDesdeExtraido(DocExtraido{Codigo: "X", Meta: map[string]string{"vigente desde": "01/01/2026",
		"relacion con otros documentos": "Complementa a PRC-001 en lo que corresponda."}}, "x.pdf", nil)
	if len(avisos) != 1 {
		t.Errorf("una relación no reconocida debería avisar: %v", avisos)
	}
}

// Un PDF de una sola página no puede detectar su encabezado/pie comparando
// páginas: lo aprende de los demás PDFs de la carpeta.
func TestRuidoEnPDFDeUnaPagina(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("sin pdftotext")
	}
	pages, err := pdftotext("corpus/07_MOD-PRC-001-01_Modificacion_y_Fe_de_Erratas.pdf", false)
	if err != nil {
		t.Fatal(err)
	}
	a := extraerDeTexto(pages[:1], "MOD-PRC-001-01", ruidoDeCarpeta("corpus"))
	d := a.Docs[0]
	if d.Titulo != "Modificación puntual N° 01 y fe de erratas a la PRC-001" {
		t.Errorf("título = %q", d.Titulo)
	}
	for _, c := range chunkear(idDoc(d.Codigo), d.Paginas) {
		if strings.Contains(c.Texto, "Documento ficticio") || strings.Contains(c.Texto, "BANCO ANDINO") {
			t.Errorf("%s con ruido: %q", c.ChunkID, recortar(c.Texto, 300))
		}
	}
}
