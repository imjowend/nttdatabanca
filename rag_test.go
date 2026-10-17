package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRegistroDelRepo(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.DocIDsDesdeTexto("¿La circular G-139-2009 sigue vigente? ¿y la Res. SBS N° 00877-2020?"); strings.Join(got, ",") != "sbs-g139-2009,sbs-877-2020" {
		t.Errorf("DocIDsDesdeTexto = %v", got)
	}
	if got := reg.DocIDsDesdeTexto("la 139-2009"); len(got) != 1 || got[0] != "sbs-g139-2009" {
		t.Errorf("número sin prefijo: %v", got)
	}
	if reg.Vigente("int-pol-continuidad-v1") != "int-pol-continuidad-v2" {
		t.Error("cadena de vigencia rota")
	}
	if g, _ := reg.Prevalece("int-proc-pruebas-bcp", "int-pol-continuidad-v2"); g != "int-pol-continuidad-v2" {
		t.Errorf("la política debería prevalecer sobre el procedimiento, ganó %q", g)
	}
	h, _ := reg.Historial("sbs-877-2020")
	if strings.Join(h.Reemplaza, ",") != "sbs-g139-2009" || strings.Join(h.Modifica, ",") != "sbs-2116-2009,sbs-272-2017" {
		t.Errorf("historial = %+v", h)
	}
	// Todos los .md del corpus se pueden chunkear.
	for _, d := range reg.Docs() {
		if filepath.Ext(d.Archivo) != ".md" {
			continue
		}
		pages, err := leerPaginas(d.Archivo)
		if err != nil {
			t.Fatal(err)
		}
		if cs := chunkear(d.ID, pages); len(cs) < 3 {
			t.Errorf("%s: pocos chunks (%d)", d.ID, len(cs))
		}
	}
}

// Pipeline completo de /ask contra fakes: valida el plumbing JSON con Jina,
// Qdrant y el LLM, el reemplazo de derogados, la validación de citas y la
// resolución de contradicciones.
func TestAskEndToEndConFakes(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	v1 := Chunk{ChunkID: "int-pol-continuidad-v1#articulo-3", DocID: "int-pol-continuidad-v1", Ruta: "Artículo 3", Pagina: 1,
		Texto: "Artículo 3.- Frecuencia de pruebas Los planes de continuidad del negocio deberán probarse como mínimo una vez al año."}
	v2 := Chunk{ChunkID: "int-pol-continuidad-v2#articulo-3", DocID: "int-pol-continuidad-v2", Ruta: "Artículo 3", Pagina: 1,
		Texto: "Artículo 3.- Frecuencia de pruebas Los planes de continuidad del negocio de los procesos críticos deberán probarse como mínimo dos veces al año."}
	proc := Chunk{ChunkID: "int-proc-pruebas-bcp#articulo-2", DocID: "int-proc-pruebas-bcp", Ruta: "Artículo 2", Pagina: 1,
		Texto: "Artículo 2.- Periodicidad Las pruebas integrales del plan de continuidad de los procesos críticos se ejecutan una vez al año, en el mes de noviembre."}

	llmJSON, _ := json.Marshal(map[string]any{
		"respuesta":     "Deben probarse como mínimo dos veces al año [int-pol-continuidad-v2#articulo-3].",
		"sin_respuesta": false,
		"citas": []map[string]string{
			{"chunk_id": v2.ChunkID, "cita_textual": "deberán probarse como mínimo dos veces al año"},
			{"chunk_id": v2.ChunkID, "cita_textual": "deberán probarse cada trimestre sin excepción"}, // alucinada
		},
		"contradicciones": []map[string]string{
			{"chunk_a": proc.ChunkID, "chunk_b": v2.ChunkID, "explicacion": "Una vez al año vs. dos veces al año."},
		},
	})

	var hibridaBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		json.Unmarshal(body, &in)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/embeddings":
			if r.Header.Get("Authorization") != "Bearer jk" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{0.1, 0.2}}}})
		case r.URL.Path == "/v1/rerank":
			json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{"index": 1, "relevance_score": 0.9}, map[string]any{"index": 0, "relevance_score": 0.7}}})
		case r.URL.Path == "/collections/normativa/points/query":
			if r.Header.Get("api-key") != "qk" {
				w.WriteHeader(403)
				return
			}
			pts := []Hit{{Score: 1, Payload: v2}} // DenseEnDocs → trae el reemplazo
			if _, ok := in["prefetch"]; ok {
				hibridaBody = in
				pts = []Hit{{Score: 0.5, Payload: proc}, {Score: 0.4, Payload: v1}}
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": pts}})
		case r.URL.Path == "/chat/completions":
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{
				map[string]any{"message": map[string]any{"content": "```json\n" + string(llmJSON) + "\n```"}}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	jinaBase = fake.URL + "/v1"

	rag := &RAG{reg: reg, jina: &Jina{key: "jk", embedModel: "m", rerankModel: "r", dim: 2},
		qd: &Qdrant{base: fake.URL, key: "qk", coll: "normativa"}, llm: &LLM{base: fake.URL, key: "lk", model: "x"}}

	resp, err := rag.Ask(context.Background(), "¿Cada cuánto se prueba el plan de continuidad?")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(hibridaBody["prefetch"].([]any)); n != 2 {
		t.Errorf("esperaba 2 prefetch (dense + bm25), hubo %d", n)
	}
	if len(resp.Citas) != 1 || resp.CitasDescartadas != 1 {
		t.Errorf("citas válidas=%d descartadas=%d", len(resp.Citas), resp.CitasDescartadas)
	}
	if len(resp.Fuentes) != 3 || !resp.Fuentes[2].Reemplazo || resp.Fuentes[2].DocID != "int-pol-continuidad-v2" {
		t.Errorf("no trajo el reemplazo de la v1 derogada: %+v", resp.Fuentes)
	}
	if len(resp.Alertas) == 0 || resp.Alertas[0].Tipo != "derogado" {
		t.Errorf("falta alerta de derogado: %+v", resp.Alertas)
	}
	if len(resp.Contradicciones) != 1 || resp.Contradicciones[0].Prevalece != "int-pol-continuidad-v2" {
		t.Errorf("contradicción mal resuelta: %+v", resp.Contradicciones)
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	os.WriteFile("ejemplo_respuesta.json", out, 0o644)
}
