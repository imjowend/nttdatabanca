package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

// ---------- Jina (embeddings + rerank) ----------

var jinaBase = "https://api.jina.ai/v1" // var (no const) para poder apuntarlo a un fake en tests

type Jina struct {
	key, embedModel, rerankModel string
	dim                          int
}

func (j *Jina) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + j.key}
}

// EmbedDocs / EmbedQuery usan task "retrieval.passage" para indexar y
// "retrieval.query" para preguntas (embeddings asimétricos: la pregunta y el
// artículo se embeben distinto).
func (j *Jina) EmbedDocs(ctx context.Context, texts []string) ([][]float32, error) {
	return j.embed(ctx, texts, "retrieval.passage")
}

func (j *Jina) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := j.embed(ctx, []string{text}, "retrieval.query")
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (j *Jina) embed(ctx context.Context, textos []string, task string) ([][]float32, error) {
	const lote = 32
	out := make([][]float32, 0, len(textos))
	for i := 0; i < len(textos); i += lote {
		fin := min(i+lote, len(textos))
		req := map[string]any{"model": j.embedModel, "task": task, "input": textos[i:fin]}
		if j.dim > 0 {
			req["dimensions"] = j.dim
		}
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := doJSON(ctx, politicaProveedor, "POST", jinaBase+"/embeddings", j.headers(), req, &resp); err != nil {
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

func (j *Jina) Rerank(ctx context.Context, query string, docs []string, topK int) ([]RerankResult, error) {
	req := map[string]any{"model": j.rerankModel, "query": query, "documents": docs, "top_n": topK, "return_documents": false}
	var resp struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := doJSON(ctx, politicaProveedor, "POST", jinaBase+"/rerank", j.headers(), req, &resp); err != nil {
		return nil, fmt.Errorf("jina rerank: %w", err)
	}
	out := make([]RerankResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Index >= 0 && r.Index < len(docs) {
			out = append(out, RerankResult{Index: r.Index, Score: r.Score})
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

func esStatus(err error, status int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == status
}

// Info devuelve la dimensión del vector "dense" de la colección, o existe=false si no está.
func (q *Qdrant) Info(ctx context.Context) (dim int, existe bool, err error) {
	var resp struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors map[string]struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := doJSON(ctx, politicaQdrant, "GET", q.url(""), q.h(), nil, &resp); err != nil {
		if esStatus(err, 404) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("qdrant info %q: %w", q.coll, err)
	}
	return resp.Result.Config.Params.Vectors["dense"].Size, true, nil
}

func errDim(coll string, actual, nueva int) error {
	return fmt.Errorf("la colección %q ya existe con dimensión %d, pero el embedder devuelve vectores de dimensión %d "+
		"(usá `ingest -reset` o cambiá QDRANT_COLLECTION)", coll, actual, nueva)
}

// Asegurar deja la colección lista para recibir vectores de dimensión dim.
//   - reset=true: si existe, loguea nombre y dimensiones vieja/nueva, la borra y la recrea.
//   - reset=false: si existe con otra dimensión, error; si no existe, la crea.
func (q *Qdrant) Asegurar(ctx context.Context, dim int, reset bool) error {
	actual, existe, err := q.Info(ctx)
	if err != nil {
		return err
	}
	if existe {
		if !reset {
			if actual != dim {
				return errDim(q.coll, actual, dim)
			}
			return nil
		}
		log.Printf("reset: borro la colección %q (dimensión vieja=%d, nueva=%d)", q.coll, actual, dim)
		if actual != dim {
			log.Printf("  aviso: cambia la dimensión de %q: %d → %d", q.coll, actual, dim)
		}
		if err := doJSON(ctx, politicaQdrant, "DELETE", q.url(""), q.h(), nil, nil); err != nil && !esStatus(err, 404) {
			return fmt.Errorf("qdrant borrar %q: %w", q.coll, err)
		}
	}
	return q.crear(ctx, dim)
}

func (q *Qdrant) crear(ctx context.Context, dim int) error {
	cfg := map[string]any{
		"vectors":        map[string]any{"dense": map[string]any{"size": dim, "distance": "Cosine"}},
		"sparse_vectors": map[string]any{"bm25": map[string]any{"modifier": "idf"}},
	}
	if err := doJSON(ctx, politicaQdrant, "PUT", q.url(""), q.h(), cfg, nil); err != nil {
		// Si un reintento llega después de que el primer PUT sí se aplicó,
		// Qdrant responde 409 "already exists": eso es éxito.
		var he *httpError
		if !errors.As(err, &he) || (he.Status != 409 && !strings.Contains(strings.ToLower(he.Body), "already exists")) {
			return fmt.Errorf("qdrant crear %q: %w", q.coll, err)
		}
		log.Printf("  la colección %q ya existía: se toma como creada", q.coll)
	}
	log.Printf("colección %q creada (dense=%d, Cosine + bm25)", q.coll, dim)
	return doJSON(ctx, politicaQdrant, "PUT", q.url("/index?wait=true"), q.h(),
		map[string]any{"field_name": "doc_id", "field_schema": "keyword"}, nil)
}

type Punto struct {
	ID      uint64         `json:"id"`
	Vector  map[string]any `json:"vector"`
	Payload Chunk          `json:"payload"`
}

// Upsert es idempotente: los IDs salen de idPunto(chunk_id) (hash determinístico),
// así que reintentar un lote reescribe los mismos puntos.
func (q *Qdrant) Upsert(ctx context.Context, pts []Punto) error {
	for i := 0; i < len(pts); i += 64 {
		fin := min(i+64, len(pts))
		if err := doJSON(ctx, politicaQdrant, "PUT", q.url("/points?wait=true"), q.h(), map[string]any{"points": pts[i:fin]}, nil); err != nil {
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
	if err := doJSON(ctx, politicaQdrant, "POST", q.url("/points/query"), q.h(), body, &resp); err != nil {
		if esStatus(err, 404) {
			return nil, fmt.Errorf("qdrant query: la colección %q no existe (¿corriste `ingest` con este EMB_PROVIDER?): %w", q.coll, err)
		}
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
