package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCitaValidaCortesYComillas(t *testing.T) {
	f := "El reglamento de ges-\n   tión de continuidad exige que “las pruebas” se ejecuten cada año en forma integral."
	casos := map[string]bool{
		"reglamento de gestión de continuidad exige":                                    true, // guion de corte de línea
		"el reglamento de gestion de continuidad exige que \"las pruebas\" se ejecuten": true, // comillas tipográficas
		"exige que «las pruebas» se ejecuten":                                           true, // « » se normalizan como “ ”
	}
	for cita, want := range casos {
		if got := citaValida(f, cita); got != want {
			t.Errorf("citaValida(%q) = %v, quiero %v", cita, got, want)
		}
	}
}

func TestCitaValidaTramos(t *testing.T) {
	f := "Las entidades deberán mantener actualizado el plan de continuidad del negocio y probarlo cada año."
	casos := map[string]bool{
		"Las entidades deberán mantener ... probarlo cada año": true,
		"probarlo cada año ... Las entidades deberán mantener": false, // orden invertido
		"Las entidades ... probarlo cada año":                  true,  // "Las entidades" = 13 chars ≥ 12
		"Las entidades deberán mantener ... cada año":          false, // tramo "cada año" < 12
	}
	for cita, want := range casos {
		if got := citaValida(f, cita); got != want {
			t.Errorf("citaValida(%q) = %v, quiero %v", cita, got, want)
		}
	}
	// Los tramos deben estar en el MISMO chunk: no se juntan entre chunks.
	if citaValida("Las entidades deberán mantener actualizado el plan", "Las entidades deberán ... probarlo cada año de forma integral") {
		t.Error("un tramo que no está en el chunk debe invalidar la cita")
	}
}

func TestCitaValidaGuionReal(t *testing.T) {
	f := "Se evalúa el impacto socio-\neconómico y el regla-\nmento aplicable a cada proceso."
	casos := map[string]bool{
		"se evalúa el impacto socio-económico":   true, // guion real al final de línea
		"se evalúa el impacto socioeconómico":    true, // también se acepta unido
		"económico y el reglamento aplicable":    true, // corte de palabra
		"económico y el regla-mento aplicable":   true, // ambigua: se acepta
		"se evalúa el impacto socio económico y": false,
	}
	for cita, want := range casos {
		if got := citaValida(f, cita); got != want {
			t.Errorf("citaValida(%q) = %v, quiero %v", cita, got, want)
		}
	}
}

func TestEsperaReintento(t *testing.T) {
	casos := []struct {
		h    string
		want time.Duration
	}{
		{"", 2 * time.Second},                            // sin header: backoff
		{"0", minRetryAfter},                             // 0 → mínimo
		{"3", 3 * time.Second},                           // valor normal
		{"3600", maxRetryAfter},                          // enorme → tope
		{"Mon, 01 Jan 2001 00:00:00 GMT", minRetryAfter}, // fecha pasada → mínimo
	}
	for _, c := range casos {
		if got := esperaReintento(c.h, 2*time.Second); got != c.want {
			t.Errorf("esperaReintento(%q) = %s, quiero %s", c.h, got, c.want)
		}
	}
}

func TestRechazoSchema(t *testing.T) {
	casos := []struct {
		he   httpError
		want bool
	}{
		{httpError{400, `Invalid value at 'response_format'`}, true},
		{httpError{400, `json_schema is not supported`}, true},
		{httpError{400, `models/gemini-x is not found`}, false},
		{httpError{400, `Invalid reasoning_effort`}, false},
		{httpError{500, `response_format`}, false},
	}
	for _, c := range casos {
		if got := rechazoSchema(&c.he); got != c.want {
			t.Errorf("rechazoSchema(%d %q) = %v, quiero %v", c.he.Status, c.he.Body, got, c.want)
		}
	}
}

func rapida(p *Politica) *Politica { c := *p; c.Base = time.Millisecond; return &c }

func TestDoJSONProveedorReintentaYRespetaRetryAfter(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	var out struct{ OK bool }
	if err := doJSON(context.Background(), rapida(politicaProveedor), "POST", srv.URL, nil, map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || n.Load() != 3 {
		t.Errorf("ok=%v intentos=%d, quiero 3", out.OK, n.Load())
	}
}

func TestPoliticaLLM(t *testing.T) {
	for _, c := range []struct {
		status, want int32
	}{{429, 2}, {503, 2}, {500, 1}, {502, 1}, {400, 1}} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(int(c.status))
		}))
		err := doJSON(context.Background(), rapida(politicaLLM), "POST", srv.URL, nil, map[string]any{}, nil)
		srv.Close()
		if err == nil || n.Load() != c.want {
			t.Errorf("status %d: intentos=%d err=%v, quiero %d intentos y error", c.status, n.Load(), err, c.want)
		}
	}
}

func TestLLMNoReintentaTimeout(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	old := httpClient
	httpClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { httpClient = old }()
	err := doJSON(context.Background(), rapida(politicaLLM), "POST", srv.URL, nil, map[string]any{}, nil)
	if err == nil || n.Load() != 1 {
		t.Errorf("timeout: intentos=%d err=%v, quiero 1 intento", n.Load(), err)
	}
}

func TestDoJSONRespetaContext(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(429)
		w.Header().Set("Retry-After", "30")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	pol := *politicaProveedor // Base 1s: la espera supera el deadline
	start := time.Now()
	err := doJSON(ctx, &pol, "POST", srv.URL, nil, map[string]any{}, nil)
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("debía cortar por context rápido: err=%v tardó %s", err, time.Since(start))
	}
	if n.Load() != 1 {
		t.Errorf("intentos=%d, quiero 1", n.Load())
	}
}

func TestQdrantCrearYaExisteEsExito(t *testing.T) {
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && r.URL.Path == "/collections/c":
			puts.Add(1)
			w.WriteHeader(409)
			w.Write([]byte(`{"status":{"error":"Collection ` + "`c`" + ` already exists!"}}`))
		case r.Method == "PUT" && r.URL.Path == "/collections/c/index":
			w.Write([]byte(`{"result":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	q := &Qdrant{base: srv.URL, key: "k", coll: "c"}
	if err := q.crear(context.Background(), 4); err != nil {
		t.Fatalf("'ya existe' debía ser éxito: %v", err)
	}
}

func TestQdrantAsegurarDimension(t *testing.T) {
	var borrado atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/collections/c":
			w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":1024}}}}}}`))
		case r.Method == "DELETE":
			borrado.Store(true)
			w.Write([]byte(`{"result":true}`))
		default:
			w.Write([]byte(`{"result":true}`))
		}
	}))
	defer srv.Close()
	q := &Qdrant{base: srv.URL, key: "k", coll: "c"}
	if err := q.Asegurar(context.Background(), 2048, false); err == nil || borrado.Load() {
		t.Errorf("sin reset y con otra dimensión debía fallar sin borrar (err=%v)", err)
	}
	if err := q.Asegurar(context.Background(), 1024, false); err != nil {
		t.Errorf("misma dimensión debía pasar: %v", err)
	}
	if err := q.Asegurar(context.Background(), 2048, true); err != nil || !borrado.Load() {
		t.Errorf("con reset debía borrar y recrear (err=%v borrado=%v)", err, borrado.Load())
	}
}
