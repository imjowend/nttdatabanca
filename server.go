package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	maxBody       = 1 << 20 // 1 MB: alcanza para una pregunta con historial
	timeoutSearch = 30 * time.Second
	// /ask puede encadenar embedding + rerank + hasta 3 llamadas al LLM
	// (fallback de json_schema y reintento por JSON inválido).
	timeoutAsk = 150 * time.Second
	// Docker manda SIGKILL 10 s después de SIGTERM salvo stop_grace_period
	// (compose.yaml lo sube a 30 s).
	timeoutApagado = 25 * time.Second
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

// statusError: 504 si se venció el timeout del request, 502 si falló un proveedor.
func statusError(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// autenticar exige "Authorization: Bearer <API_KEY>" si API_KEY está definida.
// /healthz queda abierto para el healthcheck de Docker.
func autenticar(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	esperado := []byte("Bearer " + key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), esperado) != 1 {
			errorJSON(w, http.StatusUnauthorized, errors.New("falta o es inválido el header Authorization: Bearer <API_KEY>"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func limitarBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
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
			Fecha string `json:"fecha"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
			errorJSON(w, 400, fmt.Errorf(`body esperado: {"query": "...", "k": 6, "fecha": "YYYY-MM-DD" (opcional)}`))
			return
		}
		if in.K <= 0 || in.K > 20 {
			in.K = 6
		}
		p, err := rag.Periodo(Consulta{Pregunta: in.Query, Fecha: in.Fecha})
		if err != nil {
			errorJSON(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeoutSearch)
		defer cancel()
		sr, err := rag.Search(ctx, in.Query, in.K, true, p)
		if err != nil {
			errorJSON(w, statusError(err), err)
			return
		}
		escribirJSON(w, 200, sr)
	})

	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var in Consulta
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Pregunta) == "" {
			errorJSON(w, 400, fmt.Errorf(`body esperado: {"pregunta": "...", "fecha": "YYYY-MM-DD" (opcional), "historial": [{"pregunta": "...", "respuesta": "..."}] (opcional)}`))
			return
		}
		if _, err := rag.Periodo(in); err != nil {
			errorJSON(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeoutAsk)
		defer cancel()
		resp, err := rag.Ask(ctx, in)
		if err != nil {
			errorJSON(w, statusError(err), err)
			return
		}
		escribirJSON(w, 200, resp)
	})

	// ?fecha=YYYY-MM-DD para ver el estado en otra fecha (default: la de referencia).
	periodoDeQuery := func(r *http.Request) (Periodo, error) {
		return rag.Periodo(Consulta{Fecha: r.URL.Query().Get("fecha")})
	}

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		p, err := periodoDeQuery(r)
		if err != nil {
			errorJSON(w, 400, err)
			return
		}
		type fila struct {
			*Doc
			Estado   string `json:"estado"`
			Vigencia string `json:"vigencia"`
		}
		var out []fila
		for _, d := range rag.reg.Docs() {
			out = append(out, fila{d, rag.reg.Estado(d.ID, p), rag.reg.Vigencia(d.ID)})
		}
		escribirJSON(w, 200, out)
	})

	mux.HandleFunc("GET /docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := periodoDeQuery(r)
		if err != nil {
			errorJSON(w, 400, err)
			return
		}
		h, ok := rag.reg.Historial(r.PathValue("id"), p)
		if !ok {
			errorJSON(w, 404, fmt.Errorf("documento no encontrado"))
			return
		}
		escribirJSON(w, 200, h)
	})

	srv := &http.Server{
		Addr: cfg.Addr,
		// CORS por fuera de la autenticación: el preflight OPTIONS no lleva el header.
		Handler:           cors(cfg.CORSOrigin, autenticar(cfg.APIKey, limitarBody(mux))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      timeoutAsk + 10*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	auth := "sin autenticación"
	if cfg.APIKey != "" {
		auth = "con API_KEY"
	}
	log.Printf("API escuchando en %s, %s (fecha de referencia %s)", cfg.Addr, auth, fmtFecha(rag.referencia()))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("apagando: espero hasta %s a que terminen los requests en curso", timeoutApagado)
	sctx, cancel := context.WithTimeout(context.Background(), timeoutApagado)
	defer cancel()
	return srv.Shutdown(sctx)
}

// ---------- Evals ----------
// Por defecto miden SOLO retrieval (no gastan tokens de LLM). Con -llm corren
// /ask completo y verifican la respuesta.

type CasoEval struct {
	Pregunta  string   `json:"pregunta"`
	Fecha     string   `json:"fecha,omitempty"`
	Historial []Turno  `json:"historial,omitempty"`
	Esperado  []string `json:"esperado"` // chunk_id o doc_id aceptables (alcanza con uno)
	// Solo con -llm. Cada elemento de debe_contener puede traer alternativas
	// separadas por "|" (alcanza con una); se compara sin acentos ni mayúsculas.
	DebeContener []string `json:"debe_contener,omitempty"`
	NoCitar      []string `json:"no_citar,omitempty"` // doc_id que no puede aparecer en "citas" (las vigentes)
}

func (c CasoEval) consulta() Consulta {
	return Consulta{Pregunta: c.Pregunta, Fecha: c.Fecha, Historial: c.Historial}
}

func runEval(ctx context.Context, rag *RAG, path string, k int, rerank, conLLM bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var casos []CasoEval
	if err := json.Unmarshal(data, &casos); err != nil {
		return err
	}
	var hit1, hitK, enCtx, okLLM int
	var mrr float64
	for i, c := range casos {
		p, err := rag.Periodo(c.consulta())
		if err != nil {
			return fmt.Errorf("caso %d: %w", i+1, err)
		}
		sr, err := rag.Search(ctx, queryBusqueda(c.consulta()), k, rerank, p)
		if err != nil {
			return fmt.Errorf("caso %d: %w", i+1, err)
		}
		rank, ctxOK := 0, false
		for j, res := range sr.Resultados {
			if !coincide(res, c.Esperado) {
				continue
			}
			ctxOK = true // también cuenta lo inyectado por el registro
			if j < k && res.Motivo == "" {
				rank = j + 1
			}
			break
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
		if ctxOK {
			enCtx++
		}
		fmt.Printf("%s %2d  rank=%d ctx=%v  [%s] %s\n", marca, i+1, rank, ctxOK, p, recortar(c.Pregunta, 70))
		if !conLLM {
			continue
		}
		resp, err := rag.Ask(ctx, c.consulta())
		if err != nil {
			return fmt.Errorf("caso %d: %w", i+1, err)
		}
		if fallas := verificarRespuesta(c, resp); len(fallas) > 0 {
			fmt.Printf("     ✗ LLM: %s\n       respuesta: %s\n", strings.Join(fallas, "; "), recortar(resp.Respuesta, 400))
		} else {
			okLLM++
			fmt.Printf("     ✓ LLM: %s\n", recortar(resp.Respuesta, 160))
		}
	}
	n := float64(len(casos))
	fmt.Printf("\nhit@1=%.0f%%  hit@%d=%.0f%%  MRR=%.3f  en contexto=%.0f%%  (n=%d, rerank=%v)\n",
		100*float64(hit1)/n, k, 100*float64(hitK)/n, mrr/n, 100*float64(enCtx)/n, len(casos), rerank)
	if conLLM {
		fmt.Printf("respuestas correctas: %d/%d\n", okLLM, len(casos))
	}
	return nil
}

func verificarRespuesta(c CasoEval, r *Respuesta) []string {
	var fallas []string
	texto := normalizar(r.Respuesta)
	for _, d := range c.DebeContener {
		ok := false
		for _, alt := range strings.Split(d, "|") {
			if strings.Contains(texto, normalizar(alt)) {
				ok = true
				break
			}
		}
		if !ok {
			fallas = append(fallas, fmt.Sprintf("falta %q", d))
		}
	}
	for _, ct := range r.Citas {
		if doc := docDeChunk(ct.ChunkID); slices.Contains(c.NoCitar, doc) {
			fallas = append(fallas, "cita como vigente a "+doc)
		}
	}
	return fallas
}

func coincide(r Resultado, esperados []string) bool {
	for _, e := range esperados {
		if r.ChunkID == e || r.DocID == e {
			return true
		}
	}
	return false
}
