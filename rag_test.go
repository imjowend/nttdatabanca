package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestChunkearPorArticuloYPagina(t *testing.T) {
	pages := []string{
		"RESOLUCIÓN SBS N° 877-2020\nLima, enero de 2020\nCONSIDERANDO: " + strings.Repeat("texto del considerando ", 15) +
			"\nArtículo Primero.- Aprobar el Reglamento para la Gestión de la Continuidad del Negocio.\n" +
			"Artículo 1.- Alcance\nEl presente regla-\nmento es aplicable a las empresas.\n",
		"conforme al artículo 5 del Reglamento citado.\nArtículo 2°.- Definiciones\nPara efectos del presente Reglamento...\n" +
			"ARTÍCULO DÉCIMO SEGUNDO.- Vigencia\nLa presente resolución entra en vigencia...\n",
	}
	cs := chunkear("sbs-877-2020", pages)
	var rutas []string
	for _, c := range cs {
		rutas = append(rutas, c.Ruta)
	}
	want := []string{"Preámbulo", "Artículo Primero", "Artículo 1", "Artículo 2", "Artículo Décimo Segundo"}
	if strings.Join(rutas, "|") != strings.Join(want, "|") {
		t.Fatalf("rutas = %v, quiero %v", rutas, want)
	}
	if !strings.Contains(cs[2].Texto, "reglamento es aplicable") {
		t.Errorf("no unió el guion de corte de línea: %q", cs[2].Texto)
	}
	if !strings.Contains(cs[2].Texto, "conforme al artículo 5") {
		t.Errorf("la referencia 'artículo 5 del' no debería cortar el chunk")
	}
	if cs[2].Pagina != 1 || cs[3].Pagina != 2 || cs[4].Pagina != 2 {
		t.Errorf("páginas mal: %d %d %d", cs[2].Pagina, cs[3].Pagina, cs[4].Pagina)
	}
	if cs[4].ChunkID != "sbs-877-2020#articulo-decimo-segundo" {
		t.Errorf("chunk_id = %s", cs[4].ChunkID)
	}
}

func TestPartirArticuloLargo(t *testing.T) {
	largo := "Artículo 7.- Algo\n" + strings.Repeat("Oración número con acentos áéíóú. ", 300)
	cs := chunkear("doc", []string{largo})
	if len(cs) < 3 {
		t.Fatalf("esperaba varias partes, hubo %d", len(cs))
	}
	for _, c := range cs {
		if len(c.Texto) > maxChunkChars+10 {
			t.Errorf("parte demasiado larga: %d", len(c.Texto))
		}
		if !strings.Contains(c.Ruta, "parte") {
			t.Errorf("ruta sin 'parte': %s", c.Ruta)
		}
	}
}

func TestCitaValida(t *testing.T) {
	f := "Los planes de continuidad del negocio de los procesos críticos deberán probarse como mínimo dos veces al año."
	casos := map[string]bool{
		"deberán probarse como mínimo dos veces al año":      true,
		"“DEBERAN probarse   como minimo dos veces al año.”": true,
		"Los planes de continuidad... dos veces al año":      true,
		"deberán probarse como mínimo una vez al año":        false, // parafraseo / alucinación
		"dos veces": false, // muy corta para ser evidencia
		"dos veces al año ... Los planes de continuidad del negocio": false, // fuera de orden
	}
	for cita, want := range casos {
		if got := citaValida(f, cita); got != want {
			t.Errorf("citaValida(%q) = %v, quiero %v", cita, got, want)
		}
	}
}

// Los .md del corpus demo (testdata/) se siguen pudiendo chunkear.
func TestChunkearMarkdownDemo(t *testing.T) {
	fs, _ := filepath.Glob("testdata/*.md")
	if len(fs) == 0 {
		t.Fatal("no hay .md en testdata/")
	}
	for _, f := range fs {
		pages, err := leerPaginas(f)
		if err != nil {
			t.Fatal(err)
		}
		if cs := chunkear("demo", pages); len(cs) < 3 {
			t.Errorf("%s: pocos chunks (%d)", f, len(cs))
		}
	}
}

func TestLimitesDeSeccionYAnexo(t *testing.T) {
	pages := []string{"TÍTULO I - DISPOSICIONES GENERALES\nArtículo 1°.- Objeto. Algo.\nTÍTULO II - RIESGO DE CRÉDITO\n" +
		"Artículo 2°.- Provisión. Otra cosa.\nControl de cambios\nVersión 1.0 Emisión inicial\n" +
		"ANEXO 1 - RESUMEN\nArtículo PGR-001 Materia Documento destino\nArt. 3° (a) Definición PRC-001 Art. 2°\n"}
	cs := chunkear("doc", pages)
	var got []string
	for _, c := range cs {
		got = append(got, c.ChunkID+"|"+c.Seccion)
	}
	want := []string{"doc#articulo-1|Título I - Disposiciones generales", "doc#articulo-2|Título II - Riesgo de crédito", "doc#anexo-1|"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chunks = %v, quiero %v", got, want)
	}
	if strings.Contains(cs[1].Texto, "Control de cambios") || strings.Contains(cs[1].Texto, "Emisión inicial") {
		t.Errorf("el control de cambios no debería indexarse: %q", cs[1].Texto)
	}
	if strings.Contains(cs[0].Texto, "TÍTULO II") {
		t.Errorf("el encabezado de título quedó dentro del artículo anterior: %q", cs[0].Texto)
	}
}

// fakeServicios: embeddings/rerank (Jina), Qdrant y LLM en un solo servidor.
type fakeServicios struct {
	chunks     map[string]Chunk // por chunk_id
	hibrida    []string         // chunk_ids que devuelve la query híbrida
	llmJSON    []byte
	promptLLM  string
	pedidosIDs []string
}

func (f *fakeServicios) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var in map[string]any
	json.Unmarshal(body, &in)
	w.Header().Set("Content-Type", "application/json")
	hits := func(ids []string) []Hit {
		var hs []Hit
		for i, id := range ids {
			hs = append(hs, Hit{Score: 1 - float64(i)/10, Payload: f.chunks[id]})
		}
		return hs
	}
	switch r.URL.Path {
	case "/v1/embeddings":
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{0.1, 0.2}}}})
	case "/v1/rerank":
		var rs []any
		for i := range in["documents"].([]any) {
			rs = append(rs, map[string]any{"index": i, "relevance_score": 0.9 - float64(i)/10})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": rs})
	case "/collections/normativa/points/query":
		ids := f.hibrida
		if _, ok := in["prefetch"]; !ok { // DenseEnDocs: los chunks de los docs del filtro, en orden
			ids = nil
			permitidos := in["filter"].(map[string]any)["must"].([]any)[0].(map[string]any)["match"].(map[string]any)["any"].([]any)
			for _, d := range permitidos {
				for _, id := range ordenChunks(f.chunks) {
					if f.chunks[id].DocID == d.(string) {
						ids = append(ids, id)
					}
				}
			}
			if lim := int(in["limit"].(float64)); len(ids) > lim {
				ids = ids[:lim]
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": hits(ids)}})
	case "/collections/normativa/points":
		var ids []string
		for _, x := range in["ids"].([]any) {
			for id := range f.chunks {
				if float64(idPunto(id)) == x.(float64) {
					ids = append(ids, id)
					f.pedidosIDs = append(f.pedidosIDs, id)
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"result": hits(ids)})
	case "/chat/completions":
		msgs := in["messages"].([]any)
		f.promptLLM = msgs[len(msgs)-1].(map[string]any)["content"].(string)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": "```json\n" + string(f.llmJSON) + "\n```"}}}})
	default:
		w.WriteHeader(404)
	}
}

func ordenChunks(m map[string]Chunk) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Pipeline completo de /ask contra fakes, con el registro real: inyección por
// correspondencia (PGR-001 Art. 8° → PRM-001 Art. 2°), por modificación /
// excepción (PRC-001 Art. 5° → MOD y REX) y por término redefinido (→ LIN-CAR
// Art. 2°); citas válidas, históricas y alucinadas; contradicción resuelta por fecha.
func TestAskEndToEndConFakes(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	ch := func(id, ruta, texto string) Chunk {
		return Chunk{ChunkID: id, DocID: docDeChunk(id), Ruta: ruta, Pagina: 1, Texto: texto}
	}
	f := &fakeServicios{chunks: map[string]Chunk{}}
	for _, c := range []Chunk{
		ch("pgr-001#articulo-8", "Artículo 8", "Artículo 8°.- Límite de Valor en Riesgo (VaR). El VaR diario de la cartera de negociación, calculado con un nivel de confianza de 99%, no podrá exceder el 2.5% del patrimonio efectivo."),
		ch("prm-001#articulo-2", "Artículo 2", "Artículo 2°.- VaR. El VaR diario al 99% de confianza no excede el 2.5% del patrimonio efectivo; los excesos se reportan a la Gerencia General el mismo día."),
		ch("prc-001#articulo-5", "Artículo 5", "Artículo 5°.- Reprogramación. Un crédito puede reprogramarse como máximo una (1) vez cada doce (12) meses. Las reprogramaciones de Clientes de Alto Riesgo requieren aprobación del Comité de Crédito."),
		ch("mod-prc-001-01#articulo-3", "Artículo 3", "Artículo 3°.- Fe de erratas al Artículo 5° de la PRC-001."),
		ch("rex-001-2026#articulo-5", "Artículo 5", "Artículo 5°.- Aprobación. Las reprogramaciones de Clientes de Alto Riesgo con exposición total menor a S/ 500,000 podrán ser aprobadas por la Gerencia de Agencia."),
		ch("lin-car-001-2026#articulo-2", "Artículo 2", "Artículo 2°.- Nueva definición. Se considera Cliente de Alto Riesgo a la persona natural o jurídica con atraso mayor a treinta (30) días."),
	} {
		f.chunks[c.ChunkID] = c
	}
	f.hibrida = []string{"pgr-001#articulo-8", "prc-001#articulo-5"}
	f.llmJSON, _ = json.Marshal(map[string]any{
		"respuesta":     "El VaR diario no excede el 2.5% del patrimonio efectivo [prm-001#articulo-2].",
		"sin_respuesta": false,
		"citas": []map[string]string{
			{"chunk_id": "prm-001#articulo-2", "cita_textual": "El VaR diario al 99% de confianza no excede el 2.5% del patrimonio efectivo"},
			{"chunk_id": "pgr-001#articulo-8", "cita_textual": "no podrá exceder el 2.5% del patrimonio efectivo"}, // válida pero derogada
			{"chunk_id": "prm-001#articulo-2", "cita_textual": "el VaR se revisa cada trimestre sin excepción"},    // alucinada
		},
		"contradicciones": []map[string]string{
			{"chunk_a": "pgr-001#articulo-8", "chunk_b": "prm-001#articulo-2", "explicacion": "Mismo límite en dos documentos."},
		},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	jinaBase = srv.URL + "/v1"
	j := &Jina{key: "jk", embedModel: "m", rerankModel: "r", dim: 2}
	rag := &RAG{reg: reg, emb: j, rr: j, hoy: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC),
		qd: &Qdrant{base: srv.URL, key: "qk", coll: "normativa"}, llm: &LLM{base: srv.URL, key: "lk", model: "x"}}

	resp, err := rag.Ask(context.Background(), Consulta{Pregunta: "¿Cuál es el límite de VaR?"})
	if err != nil {
		t.Fatal(err)
	}
	motivos := map[string]string{}
	for _, x := range resp.Fuentes {
		motivos[x.ChunkID] = x.Motivo
	}
	want := map[string]string{"pgr-001#articulo-8": "", "prc-001#articulo-5": "", "prm-001#articulo-2": "reemplazo",
		"lin-car-001-2026#articulo-2": "definicion", "mod-prc-001-01#articulo-3": "modificacion", "rex-001-2026#articulo-5": "excepcion"}
	for id, m := range want {
		if got, ok := motivos[id]; !ok || got != m {
			t.Errorf("fuente %s: motivo=%q presente=%v, quiero %q", id, got, ok, m)
		}
	}
	if len(resp.Citas) != 1 || resp.Citas[0].ChunkID != "prm-001#articulo-2" {
		t.Errorf("citas vigentes = %+v", resp.Citas)
	}
	if len(resp.CitasHistoricas) != 1 || resp.CitasHistoricas[0].ChunkID != "pgr-001#articulo-8" || resp.CitasDescartadas != 1 {
		t.Errorf("históricas=%+v descartadas=%d", resp.CitasHistoricas, resp.CitasDescartadas)
	}
	if len(resp.Contradicciones) != 1 || resp.Contradicciones[0].Prevalece != "prm-001" {
		t.Errorf("debería prevalecer PRM-001 (PGR-001 está derogada): %+v", resp.Contradicciones)
	}
	tipos := map[string]bool{}
	for _, a := range resp.Alertas {
		tipos[a.Tipo] = true
	}
	for _, tp := range []string{"derogado", "modificado", "excepcion", "redefinido", "temporal"} {
		if !tipos[tp] {
			t.Errorf("falta alerta %q: %+v", tp, resp.Alertas)
		}
	}
	for _, s := range []string{"FECHA DE CONSULTA: 17/10/2026", "Estado en la fecha de consulta: DEROGADO", "NOTA: El término «Cliente de Alto Riesgo»"} {
		if !strings.Contains(f.promptLLM, s) {
			t.Errorf("el prompt no contiene %q", s)
		}
	}
	if resp.FechaConsulta != "17/10/2026" {
		t.Errorf("fecha_consulta = %q", resp.FechaConsulta)
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	os.WriteFile("ejemplo_respuesta.json", out, 0o644)
}

func TestPeriodoYHistorial(t *testing.T) {
	rag := &RAG{hoy: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	casos := []struct {
		c    Consulta
		want string
	}{
		{Consulta{Pregunta: "¿Cuántas veces puedo reprogramar un crédito hoy?"}, "17/10/2026"},
		{Consulta{Pregunta: "¿Y en marzo de 2027?", Historial: []Turno{{Pregunta: "¿Cuántas veces puedo reprogramar hoy?"}}}, "01/03/2027 al 31/03/2027"},
		{Consulta{Pregunta: "¿y quién lo aprueba?", Historial: []Turno{{Pregunta: "¿Cuál era la provisión en marzo de 2026?"}}}, "01/03/2026 al 31/03/2026"},
		{Consulta{Pregunta: "¿Qué TEA tenía la línea paralela en 2024?"}, "01/01/2024 al 31/12/2024"},
		{Consulta{Pregunta: "¿Qué TEA tenía?", Fecha: "2024-07-15"}, "15/07/2024"},
	}
	for _, c := range casos {
		p, err := rag.Periodo(c.c)
		if err != nil || p.String() != c.want {
			t.Errorf("Periodo(%q) = %s, %v; quiero %s", c.c.Pregunta, p, err, c.want)
		}
	}
	if q := queryBusqueda(casos[1].c); q != "¿Cuántas veces puedo reprogramar hoy? ¿Y en marzo de 2027?" {
		t.Errorf("queryBusqueda = %q", q)
	}
	if _, err := rag.Periodo(Consulta{Pregunta: "x", Fecha: "31/02/2026"}); err == nil {
		t.Error("31/02/2026 debería ser inválida")
	}
}
