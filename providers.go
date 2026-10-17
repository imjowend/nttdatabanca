package main

import (
	"context"
	"fmt"
)

// Puertos: el resto del código (ingest, rag) depende solo de estas interfaces,
// no de un proveedor concreto. Implementaciones: Jina (clients.go), Voyage (voyage.go).

type RerankResult struct {
	Index int
	Score float64
}

type Embedder interface {
	// EmbedDocs embebe pasajes para indexar (input_type document / retrieval.passage).
	EmbedDocs(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery embebe una pregunta (input_type query / retrieval.query).
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type Reranker interface {
	// Rerank devuelve hasta topK resultados ordenados por relevancia descendente;
	// Index apunta a la posición en docs.
	Rerank(ctx context.Context, query string, docs []string, topK int) ([]RerankResult, error)
}

func nuevoEmbedder(cfg Config) (Embedder, error) {
	switch cfg.EmbProvider {
	case "voyage":
		if err := faltantes("VOYAGE_API_KEY", cfg.VoyageKey); err != nil {
			return nil, err
		}
		return &Voyage{key: cfg.VoyageKey, embedModel: cfg.VoyageEmbedModel, rerankModel: cfg.VoyageRerankModel}, nil
	case "jina":
		if err := faltantes("JINA_API_KEY", cfg.JinaKey); err != nil {
			return nil, err
		}
		return &Jina{key: cfg.JinaKey, embedModel: cfg.EmbedModel, rerankModel: cfg.RerankModel, dim: cfg.Dim}, nil
	}
	return nil, fmt.Errorf("EMB_PROVIDER=%q no soportado (jina|voyage)", cfg.EmbProvider)
}

func nuevoReranker(cfg Config) (Reranker, error) {
	switch cfg.RerankProvider {
	case "voyage":
		if err := faltantes("VOYAGE_API_KEY", cfg.VoyageKey); err != nil {
			return nil, err
		}
		return &Voyage{key: cfg.VoyageKey, embedModel: cfg.VoyageEmbedModel, rerankModel: cfg.VoyageRerankModel}, nil
	case "jina":
		if err := faltantes("JINA_API_KEY", cfg.JinaKey); err != nil {
			return nil, err
		}
		return &Jina{key: cfg.JinaKey, embedModel: cfg.EmbedModel, rerankModel: cfg.RerankModel, dim: cfg.Dim}, nil
	}
	return nil, fmt.Errorf("RERANK_PROVIDER=%q no soportado (jina|voyage)", cfg.RerankProvider)
}
