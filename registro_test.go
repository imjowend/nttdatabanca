package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func fecha(s string) Periodo { t, _ := parseFecha(s); return periodoDia(t) }

func mes(y int, m time.Month) Periodo {
	ini := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return Periodo{ini, ini.AddDate(0, 1, -1)}
}

var hoy = fecha("2026-10-17")

// Estados esperados según la tabla del 00_LEEME (fecha de referencia 17/10/2026)
// y las consultas históricas de los casos de prueba.
func TestEstadosPorFecha(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	casos := []struct {
		id   string
		p    Periodo
		want string
	}{
		{"pgr-001", hoy, "derogado"},
		{"pgr-001", fecha("2025-06-30"), "vigente"},
		{"cir-pla-007-2021", hoy, "derogado"},
		{"cir-pla-011-2022", hoy, "derogado"},
		{"cir-tc-021-2024", hoy, "caducado"},
		{"cir-tc-021-2024", mes(2024, 12), "vigente_temporal"},
		{"man-pla-001", hoy, "vigente"},
		{"rd-061-2025", hoy, "vigente"},
		{"prc-001", hoy, "modificado"},
		{"prc-001", mes(2026, 3), "vigente"},
		{"prm-001", hoy, "vigente"},
		{"pro-001", hoy, "vigente"},
		{"mod-prc-001-01", hoy, "vigente"},
		{"rex-001-2026", hoy, "vigente_temporal"},
		{"rex-001-2026", mes(2027, 3), "caducado"},
		{"rex-001-2026", mes(2026, 8), "aun_no_vigente"},
		{"lin-car-001-2026", hoy, "vigente"},
		{"lin-car-001-2026", mes(2026, 7), "aun_no_vigente"},
	}
	for _, c := range casos {
		if got := reg.Estado(c.id, c.p); got != c.want {
			t.Errorf("Estado(%s, %s) = %s, quiero %s", c.id, c.p, got, c.want)
		}
	}
	// Escisión uno a muchos.
	if got := strings.Join(reg.Reemplazos("pgr-001"), ","); got != "rd-061-2025,prc-001,prm-001,pro-001" {
		t.Errorf("Reemplazos(pgr-001) = %s", got)
	}
	if d, ok := reg.Correspondencia("pgr-001#articulo-8"); !ok || d != "prm-001#articulo-2" {
		t.Errorf("correspondencia PGR Art. 8 = %q", d)
	}
	if d, ok := reg.Correspondencia("cir-pla-011-2022#articulo-2"); !ok || d != "man-pla-001#articulo-7" {
		t.Errorf("correspondencia CIR-PLA-011 Art. 2 = %q", d)
	}
	if got := reg.DocIDsDesdeTexto("¿Sigue vigente la CIR-TC-021-2024? ¿y la mod-prc-001-01?"); strings.Join(got, ",") != "cir-tc-021-2024,mod-prc-001-01" {
		t.Errorf("DocIDsDesdeTexto = %v", got)
	}
}

func tiposAlerta(as []Alerta) string {
	var ts []string
	for _, a := range as {
		ts = append(ts, a.Tipo)
	}
	return strings.Join(ts, ",")
}

func TestAlertasPorArticulo(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	art := func(id, texto string) Chunk {
		ruta := "Artículo " + strings.TrimPrefix(id[strings.Index(id, "#"):], "#articulo-")
		return Chunk{ChunkID: id, DocID: docDeChunk(id), Ruta: ruta, Texto: texto}
	}
	prc4 := art("prc-001#articulo-4", "Se constituye una provisión genérica de 5.0% sobre la cartera clasificada como Normal.")
	prc5 := art("prc-001#articulo-5", "Las reprogramaciones de Clientes de Alto Riesgo requieren aprobación del Comité de Crédito.")
	prc3 := art("prc-001#articulo-3", "La exposición frente a un cliente no supera el 10% del patrimonio efectivo.")
	casos := []struct {
		c    Chunk
		p    Periodo
		want string
	}{
		{prc4, hoy, "modificado"},
		{prc4, mes(2026, 3), "modificacion_posterior"},
		{prc5, hoy, "modificado,excepcion,redefinido"},
		{prc5, mes(2027, 3), "modificado,excepcion_vencida,redefinido"},
		{prc5, mes(2026, 3), "modificacion_posterior"}, // ni REX ni LIN regían
		{prc3, hoy, ""}, // artículo no tocado
	}
	for _, c := range casos {
		if got := tiposAlerta(reg.AlertasChunk(c.c, c.p)); got != c.want {
			t.Errorf("AlertasChunk(%s, %s) = %q, quiero %q", c.c.ChunkID, c.p, got, c.want)
		}
	}
	if got := reg.Definiciones(prc5, hoy); strings.Join(got, ",") != "lin-car-001-2026#articulo-2" {
		t.Errorf("Definiciones = %v", got)
	}
	if got := reg.Definiciones(prc5, mes(2026, 3)); len(got) != 0 {
		t.Errorf("en marzo de 2026 LIN-CAR no regía: %v", got)
	}
	if got := tiposAlerta(reg.AlertasDoc("cir-tc-021-2024", hoy)); got != "caducado" {
		t.Errorf("AlertasDoc(cir-tc) = %q", got)
	}
	if a := reg.AlertasDoc("rex-001-2026", hoy); len(a) != 1 || !strings.Contains(a[0].Detalle, "vuelve a aplicarse PRC-001 Art. 5°") {
		t.Errorf("AlertasDoc(rex) = %+v", a)
	}
}

func TestPrevalece(t *testing.T) {
	reg, err := LoadRegistry("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	casos := []struct {
		a, b string
		p    Periodo
		want string
	}{
		{"prc-001", "rex-001-2026", hoy, "rex-001-2026"},         // excepción vigente
		{"prc-001", "rex-001-2026", mes(2027, 3), "prc-001"},     // excepción vencida
		{"prc-001", "lin-car-001-2026", hoy, "lin-car-001-2026"}, // redefinición expresa
		{"man-pla-001", "lin-car-001-2026", hoy, "lin-car-001-2026"},
		{"prc-001", "mod-prc-001-01", hoy, "mod-prc-001-01"}, // modificación
		{"pgr-001", "prm-001", hoy, "prm-001"},               // derogado pierde
		{"cir-pla-011-2022", "man-pla-001", hoy, "man-pla-001"},
		{"cir-tc-021-2024", "prc-001", hoy, "prc-001"}, // caducado pierde
	}
	for _, c := range casos {
		if got, crit := reg.Prevalece(c.a, c.b, c.p); got != c.want {
			t.Errorf("Prevalece(%s, %s, %s) = %s (%s), quiero %s", c.a, c.b, c.p, got, crit, c.want)
		}
	}
}

func TestPeriodoDesdePregunta(t *testing.T) {
	casos := map[string]string{
		"¿Cuál era la provisión genérica en marzo de 2026?":    "01/03/2026 al 31/03/2026",
		"¿Y en marzo del 2027?":                                "01/03/2027 al 31/03/2027",
		"¿Qué TEA tenía la línea paralela en 2024?":            "01/01/2024 al 31/12/2024",
		"¿Qué regía al 15/08/2026?":                            "15/08/2026",
		"¿Qué pasaba durante el año 2025?":                     "01/01/2025 al 31/12/2025",
		"¿Desde qué monto registro una operación en efectivo?": "",
		"¿Qué dice la REX-001-2026?":                           "", // el año del código no es una fecha
	}
	for q, want := range casos {
		p, ok := periodoDesdePregunta(q)
		got := ""
		if ok {
			got = p.String()
		}
		if got != want {
			t.Errorf("periodoDesdePregunta(%q) = %q, quiero %q", q, got, want)
		}
	}
}

func TestOverrides(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ov.json"
	os.WriteFile(path, []byte(`[{"doc_id": "b", "url": "https://x/b.pdf"}, {"doc_id": "c", "excluir": true}, {"doc_id": "d", "titulo": "Nuevo"}]`), 0o644)
	docs := []*Doc{{ID: "a"}, {ID: "b", Titulo: "B", Reemplaza: []string{"a"}}, {ID: "c"}}
	out, err := aplicarOverrides(docs, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[1].URL != "https://x/b.pdf" || out[1].Titulo != "B" || len(out[1].Reemplaza) != 1 || out[2].ID != "d" {
		t.Errorf("overrides mal aplicados: %+v %+v %+v", out[0], out[1], out[2])
	}
}

// registry.json tiene que coincidir con lo que genera `registro` desde los PDFs
// (más los overrides). Si falla: correr `go run . registro` y revisar el diff.
func TestRegistroAlDia(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("sin pdftotext")
	}
	docs, avisos, err := generarRegistro("corpus", ".")
	if err != nil {
		t.Fatal(err)
	}
	if docs, err = aplicarOverrides(docs, "registry.overrides.json"); err != nil {
		t.Fatal(err)
	}
	for _, a := range avisos {
		t.Errorf("aviso de generación: %s", a)
	}
	gen, _ := marshalRegistro(docs)
	actual, _ := os.ReadFile("registry.json")
	if string(gen) != string(actual) {
		t.Errorf("registry.json desactualizado: %v", diffRegistro(actual, docs))
	}
}
