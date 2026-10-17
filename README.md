# normativa-rag

RAG sobre normativa SBS + políticas internas, con citas verificadas y alertas de vigencia.
Go 1.22+, solo librería estándar. Servicios: Qdrant Cloud, Jina API, un LLM con API compatible con OpenAI.

## Requisitos
- Go 1.22+
- `pdftotext` (poppler-utils): `sudo apt install poppler-utils` o `brew install poppler`

## Puesta en marcha
```bash
cp .env.example .env   # completar keys
set -a; source .env; set +a

# 1. Bajar los PDFs a corpus/ con los nombres de registry.json (completar fecha y url)
# 2. Revisar el chunking SIN gastar créditos
go run . ingest -dry          # → chunks.jsonl
# 3. Indexar (embeddings Jina + Qdrant)
go run . ingest
# 4. Medir retrieval (no usa el LLM)
go run . eval                 # con reranker
go run . eval -norerank       # sin reranker, para comparar
# 5. API
go run . serve
```

## Endpoints
| Método | Ruta | Uso |
|---|---|---|
| POST | `/ask` `{"pregunta": "..."}` | Respuesta + citas validadas + alertas + contradicciones (forma en `ejemplo_respuesta.json`) |
| POST | `/search` `{"query": "...", "k": 6}` | Tool para el agente: chunks + metadata + alertas, sin LLM |
| GET | `/docs` | Registro con estado de cada documento |
| GET | `/docs/{id}` | Historial de versiones (reemplaza / reemplazado_por / modifica) |

## Registro de vigencia (`registry.json`)
Única fuente de verdad del estado de cada documento. Solo se carga una dirección de cada relación
(`derogado_por`, `modificado_por`); lo inverso se calcula. Cambiar estados no requiere re-ingestar,
solo reiniciar el server. Cambiar `titulo`/`numero` sí (van en el encabezado embebido).

Jerarquía para resolver contradicciones: `norma_sbs` > `basilea` > `politica_interna` > `procedimiento_interno`;
a igual nivel, prevalece la más reciente; un derogado siempre pierde.

## Docs sintéticos
`corpus/*.md` son de un "Banco Demo" ficticio, con una versión derogada (v1 → v2) y una contradicción
deliberada entre la política v2 y el procedimiento PROC-CN-07 (frecuencia y plazo de reporte de pruebas).
