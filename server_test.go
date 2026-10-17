package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type embContador struct{ llamadas int }

func (e *embContador) EmbedDocs(context.Context, []string) ([][]float32, error) { return nil, nil }

func (e *embContador) EmbedQuery(_ context.Context, t string) ([]float32, error) {
	e.llamadas++
	return []float32{float32(len(t))}, nil
}

func TestCacheEmbedder(t *testing.T) {
	base := &embContador{}
	c := conCache(base, 2)
	ctx := context.Background()
	for _, q := range []string{"a", "a", "bb", "a", "ccc", "bb"} {
		if _, err := c.EmbedQuery(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// "a" (miss) "a" (hit) "bb" (miss) "a" (hit) "ccc" (miss, saca "bb") "bb" (miss)
	if base.llamadas != 4 {
		t.Errorf("llamadas al proveedor = %d, quiero 4", base.llamadas)
	}
	if c.lru.Len() != 2 {
		t.Errorf("entradas = %d, quiero 2", c.lru.Len())
	}
}

func TestUltimosTurnos(t *testing.T) {
	var h []Turno
	for i := range 5 {
		h = append(h, Turno{Pregunta: fmt.Sprint(i)})
	}
	got := ultimosTurnos(h)
	if len(got) != maxTurnosHistorial || got[0].Pregunta != "2" {
		t.Errorf("ultimosTurnos = %+v", got)
	}
	if len(ultimosTurnos(h[:1])) != 1 {
		t.Error("con menos turnos que el máximo deben quedar todos")
	}
}

func TestAutenticarYLimiteBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var mbe *http.MaxBytesError
		if _, err := io.ReadAll(r.Body); errors.As(err, &mbe) {
			w.WriteHeader(413)
			return
		}
		w.WriteHeader(200)
	})
	h := cors("*", autenticar("secreto", limitarBody(mux)))

	casos := []struct {
		metodo, ruta, auth string
		body               string
		want               int
	}{
		{"GET", "/healthz", "", "", 200},
		{"POST", "/ask", "", "{}", 401},
		{"POST", "/ask", "Bearer otro", "{}", 401},
		{"POST", "/ask", "Bearer secreto", "{}", 200},
		{"POST", "/ask", "Bearer secreto", strings.Repeat("x", maxBody+1), 413},
		{"OPTIONS", "/ask", "", "", 204}, // preflight CORS sin header
	}
	for _, c := range casos {
		req := httptest.NewRequest(c.metodo, c.ruta, strings.NewReader(c.body))
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s auth=%q: status %d, quiero %d", c.metodo, c.ruta, c.auth, rec.Code, c.want)
		}
	}

	// Sin API_KEY la API queda abierta.
	rec := httptest.NewRecorder()
	autenticar("", mux).ServeHTTP(rec, httptest.NewRequest("POST", "/ask", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Errorf("sin API_KEY: status %d", rec.Code)
	}
}

func TestStatusError(t *testing.T) {
	if s := statusError(fmt.Errorf("llm: %w", context.DeadlineExceeded)); s != http.StatusGatewayTimeout {
		t.Errorf("timeout → %d, quiero 504", s)
	}
	if s := statusError(fmt.Errorf("HTTP 500")); s != http.StatusBadGateway {
		t.Errorf("error de proveedor → %d, quiero 502", s)
	}
}
