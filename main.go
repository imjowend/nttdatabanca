// normativa-rag: RAG sobre normativa SBS + políticas internas.
// Solo librería estándar de Go. Dependencias externas: Qdrant Cloud, Voyage AI
// (o Jina), un LLM con API compatible con OpenAI (Gemini por defecto), y
// `pdftotext` (poppler-utils) para la ingesta.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	EmbProvider, RerankProvider                    string // jina | voyage
	QdrantURL, QdrantKey, Coleccion                string
	VoyageKey, VoyageEmbedModel, VoyageRerankModel string
	JinaKey, EmbedModel, RerankModel               string
	Dim                                            int // solo Jina (parámetro "dimensions")
	LLMBase, LLMKey, LLMModel, LLMEffort           string
	PriceIn, PriceOut                              float64 // USD por millón de tokens
	RegistryPath, Addr, CORSOrigin                 string
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) (float64, error) {
	s := env(k, "")
	if s == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s=%q no es un número válido", k, s)
	}
	return f, nil
}

func cargarConfig() (Config, error) {
	dim, _ := strconv.Atoi(env("EMBED_DIM", "1024"))
	emb := strings.ToLower(env("EMB_PROVIDER", "voyage"))
	cfg := Config{
		EmbProvider:       emb,
		RerankProvider:    strings.ToLower(env("RERANK_PROVIDER", "voyage")),
		QdrantURL:         env("QDRANT_URL", ""), // ej: https://xxxx.us-east4-0.gcp.cloud.qdrant.io:6333
		QdrantKey:         env("QDRANT_API_KEY", ""),
		Coleccion:         env("QDRANT_COLLECTION", "normativa_"+emb),
		VoyageKey:         env("VOYAGE_API_KEY", ""),
		VoyageEmbedModel:  env("VOYAGE_EMBED_MODEL", "voyage-4"),
		VoyageRerankModel: env("VOYAGE_RERANK_MODEL", "rerank-2.5"),
		JinaKey:           env("JINA_API_KEY", ""),
		EmbedModel:        env("JINA_EMBED_MODEL", "jina-embeddings-v5-text-small"),
		RerankModel:       env("JINA_RERANK_MODEL", "jina-reranker-v3.5"),
		Dim:               dim,
		LLMBase:           env("LLM_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai/"),
		LLMKey:            env("LLM_API_KEY", ""),
		LLMModel:          env("LLM_MODEL", "gemini-3.8-flash"),
		LLMEffort:         strings.ToLower(env("LLM_REASONING_EFFORT", "low")),
		RegistryPath:      env("REGISTRY_PATH", "registry.json"),
		Addr:              env("ADDR", ":8080"),
		CORSOrigin:        env("CORS_ORIGIN", "*"),
	}
	var err error
	if cfg.PriceIn, err = envFloat("LLM_PRICE_IN", 0.75); err != nil {
		return cfg, err
	}
	if cfg.PriceOut, err = envFloat("LLM_PRICE_OUT", 3.75); err != nil {
		return cfg, err
	}
	return cfg, nil
}

var reGeminiVersion = regexp.MustCompile(`^gemini-(\d+)`)

// validarEffort: los modelos Gemini 3.x en adelante no permiten apagar el thinking.
func validarEffort(model, effort string) error {
	if effort != "none" {
		return nil
	}
	if m := reGeminiVersion.FindStringSubmatch(strings.ToLower(model)); m != nil {
		if v, _ := strconv.Atoi(m[1]); v >= 3 {
			return fmt.Errorf("LLM_REASONING_EFFORT=none no está soportado por %s (usá low, medium o high)", model)
		}
	}
	return nil
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

func nuevoQdrant(cfg Config) (*Qdrant, error) {
	if err := faltantes("QDRANT_URL", cfg.QdrantURL); err != nil {
		return nil, err
	}
	return &Qdrant{base: cfg.QdrantURL, key: cfg.QdrantKey, coll: cfg.Coleccion}, nil
}

func nuevoLLM(cfg Config) (*LLM, error) {
	if err := faltantes("LLM_API_KEY", cfg.LLMKey, "LLM_MODEL", cfg.LLMModel); err != nil {
		return nil, err
	}
	if err := validarEffort(cfg.LLMModel, cfg.LLMEffort); err != nil {
		return nil, err
	}
	return &LLM{base: cfg.LLMBase, key: cfg.LLMKey, model: cfg.LLMModel, effort: cfg.LLMEffort,
		priceIn: cfg.PriceIn, priceOut: cfg.PriceOut}, nil
}

// nuevoRAG valida solo lo que se va a usar: eval -norerank no pide el reranker
// y eval no pide el LLM.
func nuevoRAG(cfg Config, reg *Registry, conLLM, conRerank bool) (*RAG, error) {
	emb, err := nuevoEmbedder(cfg)
	if err != nil {
		return nil, err
	}
	q, err := nuevoQdrant(cfg)
	if err != nil {
		return nil, err
	}
	r := &RAG{reg: reg, emb: emb, qd: q}
	if conRerank {
		if r.rr, err = nuevoReranker(cfg); err != nil {
			return nil, err
		}
	}
	if conLLM {
		if r.llm, err = nuevoLLM(cfg); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func uso() {
	fmt.Fprint(os.Stderr, `uso:
  normativa-rag ingest [-dry] [-reset=true]   parsea, chunkea, embebe e indexa el corpus
  normativa-rag eval   [-f evals.json] [-k 5] [-norerank]   mide el retrieval (sin LLM)
  normativa-rag serve                          levanta la API HTTP
  normativa-rag healthcheck                    consulta /healthz local (para Docker)
`)
	os.Exit(2)
}

// healthcheck: la imagen distroless no trae curl ni shell, así que el propio
// binario consulta /healthz. No carga config ni registro: solo necesita ADDR.
func healthcheck() int {
	addr := env("ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		log.Print(err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("healthz: %s", resp.Status)
		return 1
	}
	return 0
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		uso()
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := cargarConfig()
	if err != nil {
		log.Fatal(err)
	}
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
		if rag, err = nuevoRAG(cfg, reg, false, !*norerank); err == nil {
			err = runEval(ctx, rag, *file, *k, !*norerank)
		}
	case "serve":
		var rag *RAG
		if rag, err = nuevoRAG(cfg, reg, true, true); err == nil {
			err = runServe(cfg, rag)
		}
	default:
		uso()
	}
	if err != nil {
		log.Fatal(err)
	}
}
