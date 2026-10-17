# normativa-rag

RAG sobre normativa SBS + políticas internas, con citas verificadas y alertas de vigencia.
Go 1.22+, solo librería estándar. Servicios externos: Qdrant Cloud, Voyage AI (o Jina) para embeddings y rerank, y un LLM con API compatible con OpenAI (Gemini por defecto).

## Qué hace
- Ingesta PDFs/Markdown de `corpus/`, los parte por artículo y los indexa en Qdrant.
- Responde preguntas con citas textuales **validadas** contra los chunks recuperados (las inventadas se descartan).
- Avisa cuando una fuente está **derogada** o modificada, y detecta **contradicciones** entre documentos resolviéndolas por jerarquía.

## Requisitos
- Go 1.22+
- `pdftotext` (poppler): `brew install poppler` o `sudo apt install poppler-utils`
- Cuenta/keys de: Qdrant Cloud, Voyage AI (o Jina) y un proveedor LLM (por defecto Gemini)

## Configuración
```bash
cp .env.example .env     # completar las keys
set -a; source .env; set +a
```
> El programa **no lee `.env` por sí solo**: hay que exportar las variables en cada terminal nueva (el `source` de arriba).

| Variable | Para qué |
|---|---|
| `QDRANT_URL`, `QDRANT_API_KEY` | Cluster de Qdrant Cloud (incluir el puerto `:6333`) |
| `EMB_PROVIDER`, `RERANK_PROVIDER` | `voyage` (default) o `jina` |
| `VOYAGE_API_KEY` / `JINA_API_KEY` | Key del proveedor elegido |
| `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL` | Cualquier API compatible con OpenAI |
| `LLM_REASONING_EFFORT` | `low` \| `medium` \| `high` (Gemini 3.x no admite `none`) |
| `LLM_PRICE_IN`, `LLM_PRICE_OUT` | USD por millón de tokens, solo para el log de costo |
| `REGISTRY_PATH`, `ADDR`, `CORS_ORIGIN` | Registro de vigencia, puerto (`:8080`) y CORS |

Cada comando valida solo lo que usa: `eval` no pide el LLM y `eval -norerank` no pide el reranker.

## Cómo correrlo

```bash
# 0. Verificar que compila y pasan los tests (no gasta créditos)
go vet ./... && go test ./...

# 1. Revisar el chunking SIN gastar créditos → escribe chunks.jsonl
go run . ingest -dry

# 2. Indexar (embeddings + Qdrant). Por defecto recrea la colección (-reset=true)
go run . ingest

# 3. Medir calidad del retrieval (no usa el LLM)
go run . eval                  # con reranker
go run . eval -norerank        # sin reranker, para comparar
go run . eval -k 10 -f evals.json

# 4. Levantar la API en :8080
go run . serve
```

Orden recomendado la primera vez: `-dry` → `ingest` → `eval` → `serve`.

### Con Docker
Compose lee `.env` solo (no hace falta el `source`).

```bash
docker compose run --rm ingest -dry      # chunking → ./out/chunks.jsonl
docker compose run --rm ingest           # indexar en Qdrant
docker compose run --rm api eval         # medir retrieval
docker compose up -d --build             # API en :8080 (HOST_PORT=18080 para cambiar el puerto)
```

- `api`: distroless `static` nonroot (~15 MB), sin shell; el healthcheck usa `normativa-rag healthcheck`.
- `ingest` (perfil `tools`): Alpine + `pdftotext` (~60 MB), solo se construye cuando lo usás.
- `registry.json` y `corpus/` se montan de solo lectura: cambiar estados solo pide `docker compose restart api`.
- En Linux, si `ingest` no puede escribir en `./out`, ejecutá `mkdir -p out && sudo chown 65532 out`.

## Cómo usar la API

```bash
# Pregunta completa: respuesta + citas + alertas + contradicciones
curl -s -XPOST localhost:8080/ask \
  -d '{"pregunta": "¿Cada cuánto se deben probar los planes de continuidad?"}' | jq

# Solo recuperación (sin LLM), pensado como tool para un agente
curl -s -XPOST localhost:8080/search \
  -d '{"query": "frecuencia de pruebas de continuidad", "k": 6}' | jq

# Registro de documentos y su estado
curl -s localhost:8080/docs | jq
curl -s localhost:8080/docs/int-pol-continuidad-v2 | jq   # historial de versiones
```

| Método | Ruta | Uso |
|---|---|---|
| POST | `/ask` `{"pregunta": "..."}` | Respuesta + citas validadas + alertas + contradicciones |
| POST | `/search` `{"query": "...", "k": 6}` | Chunks + metadata + alertas, sin LLM |
| GET | `/docs` | Registro con el estado de cada documento |
| GET | `/docs/{id}` | Historial (reemplaza / reemplazado_por / modifica) |

### Forma de la respuesta de `/ask`
Ejemplo completo en `ejemplo_respuesta.json`. Campos principales:
- `respuesta`: texto con citas inline `[chunk_id]`.
- `sin_respuesta`: `true` si el corpus no tiene base para responder (no alucina).
- `citas`: solo las validadas (con `cita_textual`, `documento`, `ruta`, `pagina`, `estado`); `citas_descartadas` cuenta las rechazadas.
- `alertas`: p. ej. fuente `derogado`, con el documento que la reemplaza.
- `contradicciones`: pares de chunks en conflicto, cuál `prevalece` y por qué criterio.
- `fuentes`: todos los chunks recuperados, con `score`.

## Qué probar
1. **Chunking** (`ingest -dry`): que cada artículo sea un chunk y el encabezado (título, número) quede embebido.
2. **Idempotencia**: correr `ingest -reset=false` dos veces no debe duplicar puntos en Qdrant.
3. **Retrieval** (`eval`): el rerank debería igualar o mejorar a `-norerank`.
4. **Contradicción deliberada**: preguntar frecuencia/plazo de pruebas → debe marcar política v2 vs. PROC-CN-07 y prevalecer la política.
5. **Derogado**: una pregunta que solo cubre la v1 → alerta de vigencia, no se usa como norma vigente.
6. **Fuera de corpus** (“¿capital de Francia?”): `sin_respuesta: true`.
7. **Config rota**: sin `LLM_API_KEY` o con `LLM_REASONING_EFFORT=none` en Gemini 3.x → error claro al arrancar `serve`.

## Corpus y registro de vigencia
1. Poner los PDFs en `corpus/` con los nombres de `registry.json` (completar fecha y url).
2. `registry.json` es la única fuente de verdad del estado de cada documento. Solo se carga una dirección de cada relación (`derogado_por`, `modificado_por`); lo inverso se calcula.
3. Cambiar estados **no** requiere re-ingestar, solo reiniciar el server. Cambiar `titulo`/`numero` **sí** (van en el encabezado embebido).

Jerarquía para resolver contradicciones: `norma_sbs` > `basilea` > `politica_interna` > `procedimiento_interno`; a igual nivel prevalece la más reciente; un derogado siempre pierde.

`corpus/*.md` son de un “Banco Demo” ficticio, con una versión derogada (v1 → v2) y una contradicción deliberada entre la política v2 y PROC-CN-07.

## Evals
`evals.json` es una lista de `{"pregunta": "...", "esperado": ["chunk_id", ...]}`. Agregá casos propios para medir el retrieval cuando cambies chunking, modelo o proveedor.

## Proveedores y reintentos
Cada proveedor usa su propia colección (`normativa_<proveedor>`) porque las dimensiones difieren; si la dimensión no coincide, falla con un mensaje claro. Para cambiar a Jina: `EMB_PROVIDER=jina`, `RERANK_PROVIDER=jina`, `JINA_API_KEY=...` y re-ingestar.

Reintentos (`httpx.go`): Voyage/Jina y Qdrant, 5 intentos con backoff exponencial y `Retry-After` (el upsert es idempotente: IDs determinísticos; “ya existe” al crear la colección cuenta como éxito). LLM: máximo 1 reintento, solo ante 429/503 y nunca ante timeout. Todo respeta el `context` del request.

## Problemas comunes
| Síntoma | Causa probable |
|---|---|
| `faltan variables de entorno: ...` | Olvidaste `set -a; source .env; set +a` en esta terminal |
| `pdftotext: not found` | `brew install poppler` |
| Error de dimensión al indexar | Cambiaste de proveedor/modelo: `ingest` con `-reset=true` |
| 404/“model not found” del LLM | Revisar `LLM_MODEL` y `LLM_BASE_URL` |
