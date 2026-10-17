package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

// ---------- Jina ----------

var jinaBase = "https://api.jina.ai/v1" // var (no const) para poder apuntarlo a un fake en tests

type Jina struct {
	key, embedModel, rerankModel string
	dim                          int
}

func (j *Jina) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + j.key}
}

// Embed usa task "retrieval.passage" para indexar y "retrieval.query" para preguntas
// (embeddings asimétricos: la pregunta y el artículo se embeben distinto).
func (j *Jina) Embed(ctx context.Context, textos []string, task string) ([][]float32, error) {
	const lote = 32
	out := make([][]float32, 0, len(textos))
	for i := 0; i < len(textos); i += lote {
		fin := min(i+lote, len(textos))
		req := map[string]any{"model": j.embedModel, "task": task, "dimensions": j.dim, "input": textos[i:fin]}
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := doJSON(ctx, "POST", jinaBase+"/embeddings", j.headers(), req, &resp); err != nil {
			return nil, fmt.Errorf("jina embeddings: %w", err)
		}
		if len(resp.Data) != fin-i {
			return nil, fmt.Errorf("jina embeddings: pedí %d, volvieron %d", fin-i, len(resp.Data))
		}
		sort.Slice(resp.Data, func(a, b int) bool { return resp.Data[a].Index < resp.Data[b].Index })
		for _, d := range resp.Data {
			out = append(out, d.Embedding)
		}
		if len(textos) > lote {
			log.Printf("  embeddings %d/%d", fin, len(textos))
		}
	}
	return out, nil
}

type rerankHit struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
}

func (j *Jina) Rerank(ctx context.Context, query string, docs []string, topN int) ([]rerankHit, error) {
	req := map[string]any{"model": j.rerankModel, "query": query, "documents": docs, "top_n": topN, "return_documents": false}
	var resp struct {
		Results []rerankHit `json:"results"`
	}
	if err := doJSON(ctx, "POST", jinaBase+"/rerank", j.headers(), req, &resp); err != nil {
		return nil, fmt.Errorf("jina rerank: %w", err)
	}
	out := resp.Results[:0]
	for _, r := range resp.Results {
		if r.Index >= 0 && r.Index < len(docs) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ---------- Qdrant (REST) ----------

type Qdrant struct{ base, key, coll string }

func (q *Qdrant) url(p string) string {
	return strings.TrimRight(q.base, "/") + "/collections/" + q.coll + p
}

func (q *Qdrant) h() map[string]string {
	if q.key == "" {
		return nil
	}
	return map[string]string{"api-key": q.key}
}

func (q *Qdrant) Recrear(ctx context.Context, dim int) error {
	if err := doJSON(ctx, "DELETE", q.url(""), q.h(), nil, nil); err != nil {
		var he *httpError
		if !errors.As(err, &he) || he.Status != 404 {
			return err
		}
	}
	cfg := map[string]any{
		"vectors":        map[string]any{"dense": map[string]any{"size": dim, "distance": "Cosine"}},
		"sparse_vectors": map[string]any{"bm25": map[string]any{"modifier": "idf"}},
	}
	if err := doJSON(ctx, "PUT", q.url(""), q.h(), cfg, nil); err != nil {
		return err
	}
	return doJSON(ctx, "PUT", q.url("/index?wait=true"), q.h(),
		map[string]any{"field_name": "doc_id", "field_schema": "keyword"}, nil)
}

type Punto struct {
	ID      uint64         `json:"id"`
	Vector  map[string]any `json:"vector"`
	Payload Chunk          `json:"payload"`
}

func (q *Qdrant) Upsert(ctx context.Context, pts []Punto) error {
	for i := 0; i < len(pts); i += 64 {
		fin := min(i+64, len(pts))
		if err := doJSON(ctx, "PUT", q.url("/points?wait=true"), q.h(), map[string]any{"points": pts[i:fin]}, nil); err != nil {
			return fmt.Errorf("qdrant upsert: %w", err)
		}
	}
	return nil
}

type Hit struct {
	Score   float64 `json:"score"`
	Payload Chunk   `json:"payload"`
}

func (q *Qdrant) query(ctx context.Context, body map[string]any) ([]Hit, error) {
	var resp struct {
		Result struct {
			Points []Hit `json:"points"`
		} `json:"result"`
	}
	if err := doJSON(ctx, "POST", q.url("/points/query"), q.h(), body, &resp); err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}
	return resp.Result.Points, nil
}

func filtroDocs(ids []string) map[string]any {
	return map[string]any{"must": []any{map[string]any{"key": "doc_id", "match": map[string]any{"any": ids}}}}
}

// Hibrida: dense + BM25 fusionados con RRF en una sola llamada. Si la pregunta
// nombra una resolución, suma un tercer prefetch filtrado a ese documento:
// lo empuja arriba sin excluir al resto.
func (q *Qdrant) Hibrida(ctx context.Context, dense []float32, sparse Sparse, boost []string, limit int) ([]Hit, error) {
	pre := []any{map[string]any{"query": dense, "using": "dense", "limit": limit}}
	if len(sparse.Indices) > 0 {
		pre = append(pre, map[string]any{"query": sparse, "using": "bm25", "limit": limit})
	}
	if len(boost) > 0 {
		pre = append(pre, map[string]any{"query": dense, "using": "dense", "filter": filtroDocs(boost), "limit": 10})
	}
	return q.query(ctx, map[string]any{
		"prefetch": pre, "query": map[string]any{"fusion": "rrf"}, "limit": limit, "with_payload": true,
	})
}

func (q *Qdrant) DenseEnDocs(ctx context.Context, dense []float32, ids []string, limit int) ([]Hit, error) {
	return q.query(ctx, map[string]any{
		"query": dense, "using": "dense", "filter": filtroDocs(ids), "limit": limit, "with_payload": true,
	})
}

// ---------- LLM (API compatible con OpenAI: Gemini, OpenRouter, OpenAI, Groq...) ----------

type LLM struct{ base, key, model string }

func (l *LLM) ChatJSON(ctx context.Context, system, user string) (string, error) {
	body := map[string]any{
		"model":           l.model,
		"temperature":     0,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	url := strings.TrimRight(l.base, "/") + "/chat/completions"
	if err := doJSON(ctx, "POST", url, map[string]string{"Authorization": "Bearer " + l.key}, body, &resp); err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: respuesta sin choices")
	}
	return resp.Choices[0].Message.Content, nil
}
