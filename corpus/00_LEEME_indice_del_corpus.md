# Corpus normativo de prueba: Banco Andino del Sur S.A. (ficticio)

Documentos generados para el Hack AI Agents 2026. No constituyen normativa real.
Fecha de referencia para las pruebas: 17/10/2026.

## Índice y estado esperado

| # | Código | Tipo | Vigencia | Estado esperado hoy | Relación | Caso |
|---|---|---|---|---|---|---|
| 01 | PGR-001 | Política interna | 01/04/2023 - | OBSOLETO desde 01/07/2025 | Escindida por RD-061-2025 | Base / 3 |
| 02 | CIR-PLA-007-2021 | Circular (antigua) | 01/07/2021 - | SIN EFECTO desde 01/02/2025 | Consolidada en MAN-PLA-001 | 2 |
| 03 | CIR-PLA-011-2022 | Circular (antigua) | 01/05/2022 - | SIN EFECTO desde 01/02/2025 | Consolidada en MAN-PLA-001 | 2 |
| 04 | CIR-TC-021-2024 | Circular temporal | 01/06/2024 - 31/12/2024 | CADUCADO, sin reemplazo | Ninguna | 1 |
| 05 | MAN-PLA-001 | TUO | 01/02/2025 - | VIGENTE | Reemplaza a 02 y 03 (muchos a uno) | 2 |
| 06 | RD-061-2025 (+ PRC-001, PRM-001, PRO-001) | Resolución + 3 políticas | 01/07/2025 - | VIGENTE | Reemplaza a PGR-001 (uno a muchos) | 3 |
| 07 | MOD-PRC-001-01 | Modificación / Fe de erratas | 01/05/2026 - | VIGENTE | Modifica PRC-001 Art. 4° y 5° | 4 |
| 08 | REX-001-2026 | Excepción temporal | 01/09/2026 - 28/02/2027 | VIGENTE (temporal) | Prevalece sobre PRC-001 Art. 5° | 5 |
| 09 | LIN-CAR-001-2026 | Lineamiento (glosario) | 01/08/2026 - | VIGENTE | Redefine término usado en PRC-001 y MAN-PLA-001 | 6 |

Nota: el documento 06 contiene tres políticas (Anexos A, B y C), cada una con su propio bloque de metadata. La ingesta debería registrarlas como tres documentos derivados.

## Preguntas de prueba y respuesta esperada

| Caso | Pregunta | Respuesta esperada |
|---|---|---|
| 1 | ¿Puedo ofrecer hoy una línea paralela de tarjeta? | No. CIR-TC-021-2024 caducó el 31/12/2024 sin reemplazo. |
| 1 | ¿Qué TEA tenía la línea paralela en 2024? | 39.9% (consulta histórica, CIR-TC-021-2024 Art. 4°). |
| 2 | ¿En qué plazo reporto una operación inusual? | 24 horas al Oficial de Cumplimiento (MAN-PLA-001 Art. 7°). No debe citar CIR-PLA-011-2022. |
| 2 | ¿Desde qué monto registro una operación en efectivo? | US$ 10,000 o S/ 30,000 (MAN-PLA-001 Art. 3°). |
| 3 | ¿Cuál es el límite de VaR? | 2.5% del patrimonio efectivo (PRM-001 Art. 2°). No debe citar PGR-001. |
| 3 | ¿En cuánto tiempo reporto un evento de pérdida de S/ 80,000? | 48 horas (PRO-001 Art. 2°). |
| 4 | ¿Cuál es la provisión genérica? | 5.5% desde 01/05/2026 (PRC-001 Art. 4° modificado por MOD-PRC-001-01). |
| 4 | ¿Cuál era la provisión genérica en marzo de 2026? | 5.0%. |
| 4 | ¿Quién aprueba reprogramaciones de Clientes de Alto Riesgo (regla estándar)? | Comité de Créditos (corregido por fe de erratas). |
| 5 | ¿Cuántas veces puedo reprogramar un crédito hoy? | Hasta 2 en 12 meses y 9 meses de gracia (REX-001-2026), para clientes en zonas de emergencia. |
| 5 | ¿Y en marzo de 2027? | 1 vez en 12 meses y 6 meses de gracia (PRC-001 Art. 5°). |
| 5 | ¿Quién aprueba hoy la reprogramación de un Cliente de Alto Riesgo con exposición de S/ 300,000 en zona de emergencia? | Gerencia de Agencia (REX-001-2026 Art. 5°). |
| 6 | Un cliente tiene 45 días de atraso en el último año. ¿Es Cliente de Alto Riesgo? | Sí, según LIN-CAR-001-2026 (más de 30 días). Debe alertar que PRC-001 Art. 2° aún dice 60 días. |
| 6 | ¿Un PEP requiere debida diligencia reforzada? | Sí: es Cliente de Alto Riesgo (LIN-CAR-001-2026) y aplica MAN-PLA-001 Art. 4°. |
