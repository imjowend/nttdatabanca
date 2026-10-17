package main

import (
	"container/list"
	"context"
	"fmt"
	"sync"
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

// proveedor arma el cliente de Voyage o Jina; los dos implementan Embedder y Reranker.
func proveedor(variable, nombre string, cfg Config) (interface {
	Embedder
	Reranker
}, error) {
	switch nombre {
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
	return nil, fmt.Errorf("%s=%q no soportado (jina|voyage)", variable, nombre)
}

func nuevoEmbedder(cfg Config) (Embedder, error) {
	return proveedor("EMB_PROVIDER", cfg.EmbProvider, cfg)
}

func nuevoReranker(cfg Config) (Reranker, error) {
	return proveedor("RERANK_PROVIDER", cfg.RerankProvider, cfg)
}

// cacheEmbedder guarda en memoria los embeddings de las últimas preguntas (LRU):
// repetir una pregunta (evals, reintentos del usuario) no vuelve a pagar el
// embedding. EmbedDocs (ingesta) pasa directo.
type cacheEmbedder struct {
	Embedder
	max int

	mu    sync.Mutex
	lru   *list.List               // frente = más reciente; valores *entradaEmb
	items map[string]*list.Element // texto → nodo
}

type entradaEmb struct {
	texto string
	vec   []float32
}

func conCache(e Embedder, max int) *cacheEmbedder {
	return &cacheEmbedder{Embedder: e, max: max, lru: list.New(), items: map[string]*list.Element{}}
}

func (c *cacheEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	if el, ok := c.items[text]; ok {
		c.lru.MoveToFront(el)
		vec := el.Value.(*entradaEmb).vec
		c.mu.Unlock()
		return vec, nil
	}
	c.mu.Unlock()

	// Sin el lock durante la llamada: dos requests iguales y simultáneas pueden
	// pagar dos embeddings, pero ninguna espera a la otra.
	vec, err := c.Embedder.EmbedQuery(ctx, text)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[text]; !ok {
		c.items[text] = c.lru.PushFront(&entradaEmb{text, vec})
		if c.lru.Len() > c.max {
			viejo := c.lru.Remove(c.lru.Back()).(*entradaEmb)
			delete(c.items, viejo.texto)
		}
	}
	return vec, nil
}
