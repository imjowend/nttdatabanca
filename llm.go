package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------- LLM (API compatible con OpenAI: Gemini, OpenRouter, OpenAI, Groq...) ----------

type LLM struct {
	base, key, model string
	effort           string  // reasoning_effort; vacío = no se manda
	priceIn          float64 // USD por millón de tokens de entrada
	priceOut         float64 // USD por millón de tokens de salida (incluye thinking)

	sinSchema atomic.Bool // se activa si el endpoint rechazó json_schema con 400

	mu      sync.Mutex
	nReq    int
	acumUSD float64
}

type Mensaje struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usoLLM struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// schemaRespuesta se arma desde el mismo struct que parsea rag.go, así no se desincronizan.
var schemaRespuesta = schemaDe(reflect.TypeOf(salidaLLM{}))

// schemaDe genera un JSON Schema mínimo (subconjunto que acepta Gemini):
// object/properties/required, array/items y escalares.
func schemaDe(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		var req []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			props[name] = schemaDe(f.Type)
			req = append(req, name)
		}
		return map[string]any{"type": "object", "properties": props, "required": req}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaDe(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{"type": "string"}
}

func (l *LLM) ChatJSON(ctx context.Context, system, user string) (string, error) {
	return l.Chat(ctx, []Mensaje{{Role: "system", Content: system}, {Role: "user", Content: user}})
}

// Chat pide una respuesta JSON. Intenta con response_format json_schema; si el
// endpoint lo rechaza con 400, cae a json_object y lo recuerda para la sesión.
// No manda temperature ni max_tokens: el thinking cuenta como output y un tope
// bajo trunca el JSON.
func (l *LLM) Chat(ctx context.Context, msgs []Mensaje) (string, error) {
	conSchema := !l.sinSchema.Load()
	out, err := l.chat(ctx, msgs, conSchema)
	var he *httpError
	if conSchema && errors.As(err, &he) && rechazoSchema(he) {
		log.Printf("llm: el endpoint rechazó json_schema (HTTP 400: %s); reintento con json_object",
			recortar(he.Body, 160))
		l.sinSchema.Store(true)
		out, err = l.chat(ctx, msgs, false)
	}
	return out, err
}

// rechazoSchema: solo un 400 que menciona response_format/json_schema justifica
// el fallback. Otro 400 (modelo inválido, effort, prompt largo) se devuelve tal
// cual: reintentar no lo arregla y cobraría una segunda request.
func rechazoSchema(he *httpError) bool {
	b := strings.ToLower(he.Body)
	return he.Status == 400 && (strings.Contains(b, "response_format") || strings.Contains(b, "schema"))
}

func (l *LLM) chat(ctx context.Context, msgs []Mensaje, conSchema bool) (string, error) {
	rf := map[string]any{"type": "json_object"}
	if conSchema {
		rf = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "respuesta", "schema": schemaRespuesta}}
	}
	body := map[string]any{"model": l.model, "messages": msgs, "response_format": rf}
	if l.effort != "" {
		body["reasoning_effort"] = l.effort
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usoLLM `json:"usage"`
	}
	url := strings.TrimRight(l.base, "/") + "/chat/completions"
	if err := doJSON(ctx, politicaLLM, "POST", url, map[string]string{"Authorization": "Bearer " + l.key}, body, &resp); err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	l.registrarCosto(resp.Usage)
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: respuesta sin choices")
	}
	if fr := resp.Choices[0].FinishReason; fr == "length" {
		log.Printf("llm: aviso: finish_reason=length, el JSON puede venir truncado")
	}
	return resp.Choices[0].Message.Content, nil
}

// costoUSD: si completion_tokens no incluye el thinking, la diferencia
// total - prompt sí lo incluye; se toma el mayor (el thinking se factura como output).
func (l *LLM) costoUSD(u usoLLM) (in, out int, usd float64) {
	in, out = u.PromptTokens, u.CompletionTokens
	if d := u.TotalTokens - u.PromptTokens; d > out {
		out = d
	}
	usd = float64(in)*l.priceIn/1e6 + float64(out)*l.priceOut/1e6
	return in, out, usd
}

func (l *LLM) registrarCosto(u usoLLM) {
	in, out, usd := l.costoUSD(u)
	l.mu.Lock()
	l.nReq++
	l.acumUSD += usd
	n, acum := l.nReq, l.acumUSD
	l.mu.Unlock()
	thinking := ""
	if r := u.CompletionTokensDetails.ReasoningTokens; r > 0 {
		thinking = fmt.Sprintf(" (thinking=%d)", r)
	}
	log.Printf("llm %s in=%d out=%d%s $%.6f | sesión: %d req $%.6f", l.model, in, out, thinking, usd, n, acum)
}

// Acumulado devuelve (requests, USD) de la sesión.
func (l *LLM) Acumulado() (int, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nReq, l.acumUSD
}
