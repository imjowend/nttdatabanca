// normativa-rag: RAG sobre normativa SBS + políticas internas.
// Solo librería estándar de Go. Dependencias externas: Qdrant Cloud, Jina API,
// un LLM con API compatible con OpenAI, y `pdftotext` (poppler-utils) para la ingesta.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	QdrantURL, QdrantKey, Coleccion  string
	JinaKey, EmbedModel, RerankModel string
	Dim                              int
	LLMBase, LLMKey, LLMModel        string
	RegistryPath, Addr, CORSOrigin   string
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func cargarConfig() Config {
	dim, _ := strconv.Atoi(env("EMBED_DIM", "1024"))
	return Config{
		QdrantURL:    env("QDRANT_URL", ""), // ej: https://xxxx.us-east4-0.gcp.cloud.qdrant.io:6333
		QdrantKey:    env("QDRANT_API_KEY", ""),
		Coleccion:    env("QDRANT_COLLECTION", "normativa"),
		JinaKey:      env("JINA_API_KEY", ""),
		EmbedModel:   env("JINA_EMBED_MODEL", "jina-embeddings-v5-text-small"),
		RerankModel:  env("JINA_RERANK_MODEL", "jina-reranker-v3.5"),
		Dim:          dim,
		LLMBase:      env("LLM_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai"),
		LLMKey:       env("LLM_API_KEY", ""),
		LLMModel:     env("LLM_MODEL", ""),
		RegistryPath: env("REGISTRY_PATH", "registry.json"),
		Addr:         env("ADDR", ":8080"),
		CORSOrigin:   env("CORS_ORIGIN", "*"),
	}
}

func faltantes(pares ...string) error {
	var f []string
	for i := 0; i+1 < len(pares); i += 2 {
		if pares[i+1] == "" {
			f = append(f, pares[i])
		}
	}
	if len(f) > 0 {
		return fmt.Errorf("faltan variables de entorno: %s", strings.Join(f, ", "))
	}
	return nil
}

func clientes(cfg Config) (*Jina, *Qdrant, error) {
	if err := faltantes("QDRANT_URL", cfg.QdrantURL, "JINA_API_KEY", cfg.JinaKey); err != nil {
		return nil, nil, err
	}
	return &Jina{key: cfg.JinaKey, embedModel: cfg.EmbedModel, rerankModel: cfg.RerankModel, dim: cfg.Dim},
		&Qdrant{base: cfg.QdrantURL, key: cfg.QdrantKey, coll: cfg.Coleccion}, nil
}

func nuevoRAG(cfg Config, reg *Registry, conLLM bool) (*RAG, error) {
	j, q, err := clientes(cfg)
	if err != nil {
		return nil, err
	}
	r := &RAG{reg: reg, jina: j, qd: q}
	if conLLM {
		if err := faltantes("LLM_API_KEY", cfg.LLMKey, "LLM_MODEL", cfg.LLMModel); err != nil {
			return nil, err
		}
		r.llm = &LLM{base: cfg.LLMBase, key: cfg.LLMKey, model: cfg.LLMModel}
	}
	return r, nil
}

func uso() {
	fmt.Fprint(os.Stderr, `uso:
  normativa-rag ingest [-dry] [-reset=true]   parsea, chunkea, embebe e indexa el corpus
  normativa-rag eval   [-f evals.json] [-k 5] [-norerank]   mide el retrieval (sin LLM)
  normativa-rag serve                          levanta la API HTTP
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		uso()
	}
	cmd, args := os.Args[1], os.Args[2:]
	cfg := cargarConfig()
	reg, err := LoadRegistry(cfg.RegistryPath)
	if err != nil {
		log.Fatalf("registro de vigencia: %v", err)
	}
	ctx := context.Background()

	switch cmd {
	case "ingest":
		fs := flag.NewFlagSet("ingest", flag.ExitOnError)
		dry := fs.Bool("dry", false, "solo parsea y escribe chunks.jsonl (no gasta créditos)")
		reset := fs.Bool("reset", true, "borra y recrea la colección antes de indexar")
		fs.Parse(args)
		err = runIngest(ctx, cfg, reg, *reset, *dry)
	case "eval":
		fs := flag.NewFlagSet("eval", flag.ExitOnError)
		file := fs.String("f", "evals.json", "archivo de preguntas golden")
		k := fs.Int("k", 5, "top-k a evaluar")
		norerank := fs.Bool("norerank", false, "evaluar sin reranker (para comparar)")
		fs.Parse(args)
		var rag *RAG
		if rag, err = nuevoRAG(cfg, reg, false); err == nil {
			err = runEval(ctx, rag, *file, *k, !*norerank)
		}
	case "serve":
		var rag *RAG
		if rag, err = nuevoRAG(cfg, reg, true); err == nil {
			err = runServe(cfg, rag)
		}
	default:
		uso()
	}
	if err != nil {
		log.Fatal(err)
	}
}
