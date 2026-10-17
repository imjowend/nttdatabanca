package main

import (
	"context"
	"fmt"
	"log"
	"sort"
)

// ---------- Voyage AI (embeddings + rerank) ----------
// Docs: https://docs.voyageai.com/reference/embeddings-api y /reranker-api

var voyageBase = "https://api.voyageai.com/v1" // var (no const) para apuntarlo a un fake en tests

const voyageLote = 64

type Voyage struct{ key, embedModel, rerankModel string }

func (v *Voyage) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + v.key}
}

func (v *Voyage) EmbedDocs(ctx context.Context, texts []string) ([][]float32, error) {
	return v.embed(ctx, texts, "document")
}

func (v *Voyage) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := v.embed(ctx, []string{text}, "query")
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (v *Voyage) embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	tokens := 0
	for i := 0; i < len(texts); i += voyageLote {
		fin := min(i+voyageLote, len(texts))
		req := map[string]any{"input": texts[i:fin], "model": v.embedModel, "input_type": inputType}
		var resp struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := doJSON(ctx, politicaProveedor, "POST", voyageBase+"/embeddings", v.headers(), req, &resp); err != nil {
			return nil, fmt.Errorf("voyage embeddings: %w", err)
		}
		if len(resp.Data) != fin-i {
			return nil, fmt.Errorf("voyage embeddings: pedí %d, volvieron %d", fin-i, len(resp.Data))
		}
		sort.Slice(resp.Data, func(a, b int) bool { return resp.Data[a].Index < resp.Data[b].Index })
		for j, d := range resp.Data {
			if d.Index != j {
				return nil, fmt.Errorf("voyage embeddings: índices inesperados en la respuesta (%d en la posición %d)", d.Index, j)
			}
			out = append(out, d.Embedding)
		}
		tokens += resp.Usage.TotalTokens
		if len(texts) > voyageLote {
			log.Printf("  embeddings %d/%d", fin, len(texts))
		}
	}
	if len(texts) > 1 {
		log.Printf("  voyage %s (%s): %d textos, %d tokens", v.embedModel, inputType, len(texts), tokens)
	}
	return out, nil
}

func (v *Voyage) Rerank(ctx context.Context, query string, docs []string, topK int) ([]RerankResult, error) {
	req := map[string]any{"query": query, "documents": docs, "model": v.rerankModel, "top_k": topK}
	var resp struct {
		Data []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"data"`
	}
	if err := doJSON(ctx, politicaProveedor, "POST", voyageBase+"/rerank", v.headers(), req, &resp); err != nil {
		return nil, fmt.Errorf("voyage rerank: %w", err)
	}
	out := make([]RerankResult, 0, len(resp.Data))
	for _, d := range resp.Data {
		if d.Index >= 0 && d.Index < len(docs) {
			out = append(out, RerankResult{Index: d.Index, Score: d.RelevanceScore})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out, nil
}
