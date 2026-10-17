package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var httpClient = &http.Client{Timeout: 90 * time.Second}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Politica de reintentos por cliente. Cada proveedor tiene una distinta porque
// no todas las operaciones son igual de seguras de repetir (ver abajo).
type Politica struct {
	Nombre    string
	Intentos  int            // intentos TOTALES (1 = sin reintentos)
	Base      time.Duration  // espera del 1er reintento; luego se duplica (1s, 2s, 4s, 8s...)
	Reintenta func(int) bool // qué status HTTP se reintentan
	ErrRed    bool           // reintentar errores de red / timeouts
}

func status429o5xx(s int) bool { return s == 429 || s >= 500 }

var (
	// Voyage / Jina: embeddings y rerank son idempotentes y baratos de repetir.
	politicaProveedor = &Politica{Nombre: "proveedor", Intentos: 5, Base: time.Second, Reintenta: status429o5xx, ErrRed: true}
	// Qdrant: query es de lectura, el upsert usa IDs determinísticos (idPunto) y
	// "ya existe" al crear la colección se trata como éxito en Qdrant.crear.
	politicaQdrant = &Politica{Nombre: "qdrant", Intentos: 5, Base: time.Second, Reintenta: status429o5xx, ErrRed: true}
	// LLM: cada request cuesta plata. Máximo 1 reintento, solo ante 429/503, y
	// nunca ante timeout/error de red (la request pudo haberse procesado y cobrado).
	politicaLLM = &Politica{Nombre: "llm", Intentos: 2, Base: time.Second,
		Reintenta: func(s int) bool { return s == 429 || s == 503 }, ErrRed: false}
)

// Límites a Retry-After: 0 o una fecha pasada no deben generar reintentos
// inmediatos, y un valor enorme no debe bloquear el request indefinidamente.
const (
	minRetryAfter = 500 * time.Millisecond
	maxRetryAfter = 60 * time.Second
)

// esperaReintento usa Retry-After si viene, acotado a [minRetryAfter, maxRetryAfter];
// si no, el backoff exponencial.
func esperaReintento(h string, backoff time.Duration) time.Duration {
	d, ok := retryAfter(h)
	if !ok {
		return backoff
	}
	return min(max(d, minRetryAfter), maxRetryAfter)
}

// retryAfter interpreta el header Retry-After en segundos o como fecha HTTP.
func retryAfter(h string) (time.Duration, bool) {
	if h == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(h); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// destino devuelve host+path para los logs (nunca headers ni query string).
func destino(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host + u.Path
	}
	return "?"
}

func esperar(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// doJSON hace la request respetando ctx y la política de reintentos del cliente.
func doJSON(ctx context.Context, pol *Politica, method, rawURL string, headers map[string]string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	intentos := max(pol.Intentos, 1)
	espera := pol.Base
	for intento := 1; ; intento++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // cancelado por el caller: nunca reintentar
			}
			if pol.ErrRed && intento < intentos {
				log.Printf("↻ [%s] reintento %d/%d %s %s: %v, espero %s",
					pol.Nombre, intento+1, intentos, method, destino(rawURL), errSinURL(err), espera)
				if err := esperar(ctx, espera); err != nil {
					return err
				}
				espera *= 2
				continue
			}
			return errSinURL(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if pol.Reintenta != nil && pol.Reintenta(resp.StatusCode) && intento < intentos {
			w := esperaReintento(resp.Header.Get("Retry-After"), espera)
			log.Printf("↻ [%s] reintento %d/%d %s %s: HTTP %d, espero %s",
				pol.Nombre, intento+1, intentos, method, destino(rawURL), resp.StatusCode, w)
			if err := esperar(ctx, w); err != nil {
				return err
			}
			espera *= 2
			continue
		}
		if resp.StatusCode >= 300 {
			return &httpError{Status: resp.StatusCode, Body: recortar(string(data), 400)}
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

// errSinURL quita la URL completa de los *url.Error (por si alguna vez lleva
// credenciales en la query) y deja host+path.
func errSinURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, destino(ue.URL), ue.Err)
	}
	return err
}

func recortar(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
