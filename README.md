# normativa-rag

RAG sobre normativa y políticas internas de un banco, con citas verificadas y **vigencia por fecha**.
Go 1.22+, solo librería estándar. Servicios externos: Qdrant Cloud, Voyage AI (o Jina) para embeddings y rerank, y un LLM con API compatible con OpenAI (Gemini por defecto).

Corpus actual: los PDFs del Hack AI Agents 2026 (Banco Andino del Sur S.A., ficticio) en `corpus/`.

> **Próximo cambio:** Qdrant se va a reemplazar por **PostgreSQL + pgvector**. Todavía no está aplicado: hoy todo lo que se describe abajo usa Qdrant. Ver [Migración a pgvector](#migración-a-pgvector-planificada).

## Qué hace
- **Lee los PDFs** con una plantilla común (bloque de metadatos + artículos + anexos), descarta encabezados, pies, firmas y el control de cambios, y los parte por artículo y anexo. Un `ANEXO A - … (PRC-001)` con metadatos propios se registra como **documento derivado**.
- **Genera el registro de vigencia** (`registro`) desde los metadatos: vigente desde/hasta, reemplazos (uno a uno, muchos a uno, uno a muchos), modificaciones de artículos puntuales, excepciones temporales, redefiniciones de términos y tablas de correspondencia de artículos.
- **Responde según una fecha de consulta**: hoy por defecto, o la que diga la pregunta ("en marzo de 2026", "en 2024") o el request. El estado de cada documento (vigente, caducado, derogado, temporal, todavía no vigente) se calcula a esa fecha.
- **Trae el contexto que manda el registro** aunque el ranking no lo traiga: el artículo que reemplaza a uno derogado, la modificación o excepción que afecta a un artículo y la definición vigente de un término redefinido.
- **Valida las citas** contra los chunks (las inventadas se descartan) y separa las de documentos que no rigen en esa fecha (`citas_historicas`).
- **Resuelve las contradicciones con código, no con el LLM**: pierde lo que no rige; después gana una relación expresa (excepción, redefinición, modificación, reemplazo); después la norma posterior.

### Pipeline de `/ask`
1. **Fecha de consulta**: `fecha` del request > fecha en la pregunta > fecha en el turno anterior del historial > `FECHA_REFERENCIA`.
2. **Búsqueda híbrida** en una sola query a Qdrant: dense (Voyage/Jina) + BM25 (sparse, IDF calculado por Qdrant) fusionados con RRF, 30 candidatos. Si la pregunta nombra un documento (`PRC-001`), un tercer prefetch filtrado a ese documento lo empuja arriba.
3. **Rerank** de los candidatos con el encabezado contextual (documento + sección + artículo) → top 8.
4. **Contexto del registro**: se agregan los chunks de reemplazo, modificación, excepción y definición que correspondan (por ID y por similitud dentro del documento, en paralelo y reusando el mismo embedding).
5. **Una sola llamada al LLM** (JSON con `json_schema`, fallback a `json_object`) que responde y señala contradicciones.
6. **Validación en código**: citas literales contra el chunk, separación vigentes/históricas y resolución de qué prevalece.

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
| `QDRANT_COLLECTION` | Default `normativa_<EMB_PROVIDER>` |
| `EMB_PROVIDER`, `RERANK_PROVIDER` | `voyage` (default) o `jina` |
| `VOYAGE_API_KEY` / `JINA_API_KEY` | Key del proveedor elegido |
| `VOYAGE_EMBED_MODEL`, `VOYAGE_RERANK_MODEL` | Default `voyage-4` y `rerank-2.5` |
| `JINA_EMBED_MODEL`, `JINA_RERANK_MODEL`, `EMBED_DIM` | Default `jina-embeddings-v5-text-small`, `jina-reranker-v3.5` y `1024` (solo Jina) |
| `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL` | Cualquier API compatible con OpenAI (default: Gemini, `gemini-3.8-flash`) |
| `LLM_REASONING_EFFORT` | `low` \| `medium` \| `high` (Gemini 3.x no admite `none`) |
| `LLM_PRICE_IN`, `LLM_PRICE_OUT` | USD por millón de tokens, solo para el log de costo |
| `FECHA_REFERENCIA` | "Hoy" para la vigencia (`2026-10-17` en la hackathon). Vacío = fecha del sistema |
| `REGISTRY_PATH`, `ADDR`, `CORS_ORIGIN` | Registro de vigencia, puerto (`:8080`) y CORS |
| `API_KEY` | Si se define, la API exige `Authorization: Bearer <API_KEY>` (salvo `/healthz`). Vacío = sin autenticación |

Cada comando valida solo lo que usa: `registro` no pide keys, `eval` no pide el LLM y `eval -norerank` no pide el reranker.

## Cómo correrlo

```bash
# 0. Verificar que compila y pasan los tests (no gasta créditos; lee los PDFs reales)
go vet ./... && go test ./...

# 1. Generar el registro de vigencia desde los PDFs (no gasta créditos)
go run . registro -dry          # muestra lo detectado y el diff contra registry.json
go run . registro               # escribe registry.json → revisar el diff y commitear

# 2. Revisar el chunking SIN gastar créditos → escribe chunks.jsonl
go run . ingest -dry

# 3. Indexar (embeddings + Qdrant). Por defecto recrea la colección (-reset=true)
go run . ingest

# 4. Medir calidad
go run . eval                  # retrieval con reranker (no usa el LLM)
go run . eval -norerank        # sin reranker, para comparar
go run . eval -llm             # además corre /ask y verifica cada respuesta (~14 requests al LLM)

# 5. Levantar la API en :8080
go run . serve
```

Orden recomendado la primera vez: `registro -dry` → `registro` → `ingest -dry` → `ingest` → `eval` → `eval -llm` → `serve`.

### Agregar un PDF nuevo (mismo formato)
1. Copiarlo en `corpus/` (por ejemplo `corpus/10_XXX-001-2026_Algo.pdf`).
2. `go run . registro -dry`: revisar que el documento y sus relaciones aparezcan bien y que no haya avisos (`!`).
3. Si alguna relación no se entendió, corregirla en `registry.overrides.json` (ver abajo) y repetir el paso 2.
4. `go run . registro && go run . ingest`, y agregar sus preguntas a `evals.json`.

### Con Docker
Compose lee `.env` solo (no hace falta el `source`).

```bash
docker compose run --rm ingest -dry      # chunking → ./out/chunks.jsonl
docker compose run --rm ingest           # indexar en Qdrant
docker compose run --rm api eval         # medir retrieval
docker compose up -d --build             # API en :8080 (HOST_PORT=18080 para cambiar el puerto)
# registro desde Docker (escribe en ./out; después copiarlo a registry.json):
docker compose run --rm --entrypoint /app/normativa-rag ingest registro -corpus /app/corpus -o /app/out/registry.json
```

- `api`: distroless `static` nonroot (~15 MB), sin shell; el healthcheck usa `normativa-rag healthcheck`.
- `ingest` (perfil `tools`): Alpine + `pdftotext` (~60 MB), solo se construye cuando lo usás.
- `registry.json` y `corpus/` se montan de solo lectura: cambiar el registro solo pide `docker compose restart api`.
- En Linux, si `ingest` no puede escribir en `./out`, ejecutá `mkdir -p out && sudo chown 65532 out`.

## Cómo usar la API

```bash
# Pregunta completa: respuesta + citas + alertas + contradicciones
curl -s -XPOST localhost:8080/ask \
  -d '{"pregunta": "¿Cuántas veces puedo reprogramar un crédito hoy?"}' | jq

# Repregunta con historial y fecha detectada en la pregunta
curl -s -XPOST localhost:8080/ask -d '{"pregunta": "¿Y en marzo de 2027?",
  "historial": [{"pregunta": "¿Cuántas veces puedo reprogramar un crédito hoy?", "respuesta": "..."}]}' | jq

# Fecha explícita (pisa la detectada)
curl -s -XPOST localhost:8080/ask -d '{"pregunta": "¿Cuál es la provisión genérica?", "fecha": "2026-03-15"}' | jq

# Solo recuperación (sin LLM), pensado como tool para un agente
curl -s -XPOST localhost:8080/search -d '{"query": "límite de VaR", "k": 6}' | jq

# Registro y estado de cada documento (hoy o ?fecha=YYYY-MM-DD)
curl -s localhost:8080/docs | jq
curl -s 'localhost:8080/docs/prc-001?fecha=2027-03-15' | jq
```

| Método | Ruta | Uso |
|---|---|---|
| POST | `/ask` `{"pregunta", "fecha"?, "historial"?}` | Respuesta + citas validadas + alertas + contradicciones |
| POST | `/search` `{"query", "k"?, "fecha"?}` | Chunks + metadata + alertas, sin LLM |
| GET | `/docs[?fecha=]` | Registro con el estado de cada documento en esa fecha |
| GET | `/docs/{id}[?fecha=]` | Historial: reemplaza / reemplazado_por / modifica / prevalece_sobre / derivados |
| GET | `/healthz` | `{"status": "ok"}` (lo usa `normativa-rag healthcheck` en Docker) |

`/search` acepta `k` entre 1 y 20 (default 6). Los errores van como `{"error": "..."}`: 400 validación, 401 falta la API key, 502 falló un proveedor externo y 504 se venció el tiempo del request.

Con `API_KEY` definida, agregar `-H "Authorization: Bearer $API_KEY"` a los `curl` de arriba.

**Límites del servidor**: body de hasta 1 MB; `/search` corta a los 30 s y `/ask` a los 150 s (embedding + rerank + hasta 3 llamadas al LLM). Ante SIGTERM (`docker compose stop`) deja de aceptar conexiones y espera hasta 25 s a que terminen las que están en curso.

**Costo**: los embeddings de las últimas 256 preguntas quedan en memoria (repetir una pregunta no vuelve a pagar el embedding), y a `/ask` solo llegan los últimos 3 turnos del `historial`, recortados a 2000 caracteres cada uno.

### Forma de la respuesta de `/ask`
Ejemplo completo en `ejemplo_respuesta.json` (lo regenera `go test`). Campos principales:
- `fecha_consulta`: el día o periodo contra el que se evaluó la vigencia.
- `respuesta`: texto con citas inline `[chunk_id]`.
- `sin_respuesta`: `true` si el corpus no tiene base para responder (no alucina).
- `citas`: validadas y de documentos que rigen en la fecha; `citas_historicas`: validadas pero de documentos derogados o caducados; `citas_descartadas` cuenta las rechazadas.
- `alertas`: `derogado`, `caducado`, `aun_no_vigente`, `temporal`, `modificado`, `modificacion_posterior`, `excepcion`, `excepcion_vencida`, `redefinido`. Las de artículo traen `chunk_id`.
- `contradicciones`: pares de chunks en conflicto, cuál `prevalece` y por qué criterio.
- `fuentes`: los chunks del contexto, con `estado`, `vigencia` y `score`. Los que agregó el registro traen `motivo` (`reemplazo`, `modificacion`, `excepcion`, `definicion`) y score 0.

## Registro de vigencia
`registry.json` es la única fuente de verdad sobre la vigencia; Qdrant solo guarda texto. **Se genera** con `registro` desde el bloque de metadatos de cada PDF:

| En el PDF | En el registro |
|---|---|
| Vigente desde / Vigente hasta | `vigente_desde`, `vigente_hasta` (temporal → caduca después) |
| "Reemplaza a X", "deja sin efecto X" | `reemplaza: [x]` (X deja de regir desde el `vigente_desde` del que lo reemplaza) |
| "Modifica a X: Art. 4° y Art. 5°" | `modifica: [{doc: x, articulos: [4, 5]}]` |
| "Prevalece temporalmente sobre X Art. 5°" | `prevalece_sobre: [{doc: x, articulos: [5]}]` (solo mientras rige) |
| "cambia la interpretación del término «T» en X y Y" | `redefine: [{termino, chunk, docs}]` |
| Tablas "Art. N° → Art. M°" (distribución, correspondencia) | `correspondencias: {"origen#articulo-N": "destino#articulo-M"}` |
| ANEXO con metadatos propios | documento con `padre` |

Las relaciones se cargan en el documento que actúa; las inversas se calculan al cargar. Lo que la generación no entienda lo avisa con `!`, y se corrige en **`registry.overrides.json`**: una lista de objetos parciales con `doc_id` cuyos campos pisan a los generados (`"excluir": true` saca un documento; un `doc_id` nuevo se agrega). Ejemplo: `[{"doc_id": "prc-001", "url": "https://intranet/prc-001.pdf"}]`.

Cambiar el registro **no** requiere re-ingestar, solo reiniciar el server. Cambiar `titulo`/`numero` **sí** (van en el encabezado embebido). `TestRegistroAlDia` falla si `registry.json` no coincide con lo que generan los PDFs.

`corpus/00_LEEME_indice_del_corpus.md` es el índice de la hackathon con las respuestas esperadas: no es un PDF, no está en el registro y nunca se indexa. El corpus demo anterior ("Banco Demo", `.md`) quedó en `testdata/` para los tests.

## Evals
`evals.json` tiene los 14 casos del `00_LEEME`. Cada caso: `{"pregunta", "esperado": [chunk_id o doc_id], "fecha"?, "historial"?, "debe_contener"?, "no_citar"?}`.
- `eval` mide el retrieval: hit@1, hit@k, MRR y `en contexto` (si el chunk esperado llegó al prompt, incluido lo que inyecta el registro).
- `eval -llm` además corre `/ask`: cada elemento de `debe_contener` admite alternativas con `|` (sin acentos ni mayúsculas), y `no_citar` falla si ese documento aparece en `citas` (las vigentes).

## Proveedores y reintentos
Cada proveedor usa su propia colección (`normativa_<proveedor>`) porque las dimensiones difieren; si la dimensión no coincide, falla con un mensaje claro. Para cambiar a Jina: `EMB_PROVIDER=jina`, `RERANK_PROVIDER=jina`, `JINA_API_KEY=...` y re-ingestar.

Reintentos (`httpx.go`): Voyage/Jina y Qdrant, 5 intentos con backoff exponencial y `Retry-After` (el upsert es idempotente: IDs determinísticos; "ya existe" al crear la colección cuenta como éxito). LLM: máximo 1 reintento, solo ante 429/503 y nunca ante timeout. Todo respeta el `context` del request.

## Migración a pgvector (planificada)
Qdrant se va a reemplazar por PostgreSQL + pgvector. **Todavía no está implementado**; esta sección solo anticipa el alcance:
- El acceso al índice está concentrado en `clients.go` (`Qdrant`: `Asegurar`, `Upsert`, `Hibrida`, `DenseEnDocs`, `PorIDs`, `Info`). La migración reemplaza ese cliente; el registro de vigencia, el chunking, el rerank y la validación de citas no cambian.
- La búsqueda híbrida hoy la resuelve Qdrant (dense + sparse BM25 + RRF). En Postgres habrá que armarla en SQL: `vector` con índice HNSW para dense, full-text (`tsvector` con configuración `spanish`) o `sparsevec` para la parte léxica, y la fusión RRF en la query.
- Las variables `QDRANT_*` se reemplazarán por la conexión a Postgres, y habrá que re-ingestar el corpus.
- Como el proyecto usa solo la librería estándar, hará falta agregar un driver de Postgres (por ejemplo `pgx`) o hablar con la base por otra vía.

## Problemas comunes
| Síntoma | Causa probable |
|---|---|
| `faltan variables de entorno: ...` | Olvidaste `set -a; source .env; set +a` en esta terminal |
| `pdftotext: not found` | `brew install poppler` |
| `! X está en corpus/… pero no en el registro` | Agregaste un PDF: correr `registro` antes de `ingest` |
| `! no entendí la relación …` | Cargar esa relación en `registry.overrides.json` |
| `TestRegistroAlDia` falla | Cambiaron los PDFs u overrides: `go run . registro` y revisar el diff |
| Estados raros ("caducado" en algo vigente) | Revisar `FECHA_REFERENCIA` |
| Error de dimensión al indexar | Cambiaste de proveedor/modelo: `ingest` con `-reset=true` |
| 404/"model not found" del LLM | Revisar `LLM_MODEL` y `LLM_BASE_URL` |
