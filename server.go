package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

func escribirJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func errorJSON(w http.ResponseWriter, status int, err error) {
	escribirJSON(w, status, map[string]string{"error": err.Error()})
}

func cors(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runServe(cfg Config, rag *RAG) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		escribirJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Tool para el agente del equipo: chunks + metadata + alertas, sin LLM.
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
			errorJSON(w, 400, fmt.Errorf(`body esperado: {"query": "...", "k": 6}`))
			return
		}
		if in.K <= 0 || in.K > 20 {
			in.K = 6
		}
		sr, err := rag.Search(r.Context(), in.Query, in.K, true)
		if err != nil {
			errorJSON(w, 502, err)
			return
		}
		escribirJSON(w, 200, sr)
	})

	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pregunta string `json:"pregunta"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Pregunta) == "" {
			errorJSON(w, 400, fmt.Errorf(`body esperado: {"pregunta": "..."}`))
			return
		}
		resp, err := rag.Ask(r.Context(), in.Pregunta)
		if err != nil {
			errorJSON(w, 502, err)
			return
		}
		escribirJSON(w, 200, resp)
	})

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		type fila struct {
			*Doc
			Estado string `json:"estado"`
		}
		var out []fila
		for _, d := range rag.reg.Docs() {
			out = append(out, fila{d, rag.reg.Estado(d.ID)})
		}
		escribirJSON(w, 200, out)
	})

	mux.HandleFunc("GET /docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		h, ok := rag.reg.Historial(r.PathValue("id"))
		if !ok {
			errorJSON(w, 404, fmt.Errorf("documento no encontrado"))
			return
		}
		escribirJSON(w, 200, h)
	})

	log.Printf("API escuchando en %s", cfg.Addr)
	return http.ListenAndServe(cfg.Addr, cors(cfg.CORSOrigin, mux))
}

// ---------- Evals: miden SOLO retrieval, no gastan tokens de LLM ----------

type CasoEval struct {
	Pregunta string   `json:"pregunta"`
	Esperado []string `json:"esperado"` // chunk_id o doc_id aceptables
}

func runEval(ctx context.Context, rag *RAG, path string, k int, rerank bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var casos []CasoEval
	if err := json.Unmarshal(data, &casos); err != nil {
		return err
	}
	var hit1, hitK int
	var mrr float64
	for i, c := range casos {
		sr, err := rag.Search(ctx, c.Pregunta, k, rerank)
		if err != nil {
			return fmt.Errorf("caso %d: %w", i+1, err)
		}
		rank := 0
		for j, res := range sr.Resultados {
			if j >= k {
				break
			}
			if coincide(res, c.Esperado) {
				rank = j + 1
				break
			}
		}
		marca := "✗"
		if rank > 0 {
			marca = "✓"
			hitK++
			mrr += 1 / float64(rank)
			if rank == 1 {
				hit1++
			}
		}
		fmt.Printf("%s %2d  rank=%d  %s\n", marca, i+1, rank, recortar(c.Pregunta, 80))
	}
	n := float64(len(casos))
	fmt.Printf("\nhit@1=%.0f%%  hit@%d=%.0f%%  MRR=%.3f  (n=%d, rerank=%v)\n",
		100*float64(hit1)/n, k, 100*float64(hitK)/n, mrr/n, len(casos), rerank)
	return nil
}

func coincide(r Resultado, esperados []string) bool {
	for _, e := range esperados {
		if r.ChunkID == e || r.DocID == e {
			return true
		}
	}
	return false
}
