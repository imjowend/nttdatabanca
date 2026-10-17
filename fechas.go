package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Periodo es un intervalo de días, ambos extremos inclusive. "Hoy" es un
// periodo de un día; "en marzo de 2026" es el mes completo. Un documento rige
// en el periodo si su vigencia se superpone con él.
type Periodo struct {
	Desde, Hasta time.Time
}

func dia(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }

func periodoDia(t time.Time) Periodo { t = dia(t); return Periodo{t, t} }

func (p Periodo) String() string {
	if p.Desde.Equal(p.Hasta) {
		return fmtFecha(p.Desde)
	}
	return fmtFecha(p.Desde) + " al " + fmtFecha(p.Hasta)
}

func fmtFecha(t time.Time) string { return t.Format("02/01/2006") }

// fmtISO convierte "2025-07-01" a "01/07/2025" para mostrar; si no parsea, la deja igual.
func fmtISO(s string) string {
	if t, ok := parseFecha(s); ok {
		return fmtFecha(t)
	}
	return s
}

var (
	reFechaDMY = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4})\b`)
	reFechaISO = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
)

// parseFecha acepta dd/mm/yyyy y yyyy-mm-dd.
func parseFecha(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	var y, m, d int
	if g := reFechaISO.FindStringSubmatch(s); g != nil {
		y, _ = strconv.Atoi(g[1])
		m, _ = strconv.Atoi(g[2])
		d, _ = strconv.Atoi(g[3])
	} else if g := reFechaDMY.FindStringSubmatch(s); g != nil && g[0] == s {
		d, _ = strconv.Atoi(g[1])
		m, _ = strconv.Atoi(g[2])
		y, _ = strconv.Atoi(g[3])
	} else {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != m {
		return time.Time{}, false // 31/02 y similares
	}
	return t, true
}

func isoFecha(t time.Time) string { return t.Format("2006-01-02") }

var meses = map[string]time.Month{"enero": 1, "febrero": 2, "marzo": 3, "abril": 4, "mayo": 5, "junio": 6, "julio": 7,
	"agosto": 8, "septiembre": 9, "setiembre": 9, "octubre": 10, "noviembre": 11, "diciembre": 12}

var (
	reMesAnio = regexp.MustCompile(`\b(?:en|de|durante|para)\s+(enero|febrero|marzo|abril|mayo|junio|julio|agosto|septiembre|setiembre|octubre|noviembre|diciembre)(?:\s+(?:de|del))?\s+((?:19|20)\d{2})\b`)
	reAnio    = regexp.MustCompile(`\b(?:en|durante|para)\s+(?:el\s+)?(?:ano\s+)?((?:19|20)\d{2})\b`)
)

// periodoDesdePregunta detecta la fecha de la consulta en el texto: "al
// 15/03/2026", "en marzo de 2026", "en 2024". ok=false si no menciona ninguna
// ("hoy", "actualmente" o nada): el caller usa la fecha de referencia.
func periodoDesdePregunta(q string) (Periodo, bool) {
	s := sinAcentos(q)
	if g := reFechaDMY.FindStringSubmatch(s); g != nil {
		if t, ok := parseFecha(g[0]); ok {
			return periodoDia(t), true
		}
	}
	if g := reMesAnio.FindStringSubmatch(s); g != nil {
		y, _ := strconv.Atoi(g[2])
		ini := time.Date(y, meses[g[1]], 1, 0, 0, 0, 0, time.UTC)
		return Periodo{ini, ini.AddDate(0, 1, -1)}, true
	}
	if g := reAnio.FindStringSubmatch(s); g != nil {
		y, _ := strconv.Atoi(g[1])
		return Periodo{time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC)}, true
	}
	return Periodo{}, false
}

// fechaReferencia: FECHA_REFERENCIA (para pruebas reproducibles) u hoy.
func fechaReferencia(s string) (time.Time, error) {
	if s == "" {
		return dia(time.Now()), nil
	}
	t, ok := parseFecha(s)
	if !ok {
		return time.Time{}, fmt.Errorf("FECHA_REFERENCIA=%q no es una fecha válida (YYYY-MM-DD o dd/mm/yyyy)", s)
	}
	return t, nil
}
