package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

var httpClient = &http.Client{Timeout: 90 * time.Second}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// doJSON hace la request y reintenta con backoff exponencial ante 429 / 5xx
// (la key free de Jina tiene límites bajos de tokens por minuto).
func doJSON(ctx context.Context, method, url string, headers map[string]string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	espera := time.Second
	for intento := 0; ; intento++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
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
			if intento < 3 && ctx.Err() == nil {
				time.Sleep(espera)
				espera *= 2
				continue
			}
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && intento < 5 {
			w := espera
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				w = time.Duration(s) * time.Second
			}
			select {
			case <-time.After(w):
			case <-ctx.Done():
				return ctx.Err()
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

func recortar(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
