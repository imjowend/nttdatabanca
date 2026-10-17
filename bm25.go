package main

import (
	"hash/fnv"
	"sort"
	"strings"
	"unicode"
)

// BM25 "partido": acá calculamos solo la parte de TF; el IDF lo calcula Qdrant
// porque la colección se crea con sparse vector modifier "idf".

var acentos = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
	"à", "a", "è", "e", "ì", "i", "ò", "o", "ù", "u")

func sinAcentos(s string) string { return acentos.Replace(strings.ToLower(s)) }

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a al algo algun alguna algunas alguno algunos ante antes aquel asi aun bajo
		cada como con contra cual cuales cuando de del desde donde dos e el ella ellas ellos en entre era es esa esas
		ese eso esos esta estas este esto estos fue ha han hasta hay la las le les lo los mas me mi mientras muy ni
		nos o otra otras otro otros para pero por que quien se segun ser si sin sino sobre son su sus tal tambien
		tanto te todo todos tu un una uno unos y ya`) {
		stopwords[w] = true
	}
}

// raiz es un stemming mínimo: no es Snowball, pero junta plurales comunes.
func raiz(t string) string {
	if len(t) > 5 && strings.HasSuffix(t, "iones") {
		return t[:len(t)-2] // provisiones → provision
	}
	if len(t) > 4 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "is") {
		return t[:len(t)-1] // riesgos → riesgo (análisis/crisis quedan igual)
	}
	return t
}

func tokenizar(s string) []string {
	campos := strings.FieldsFunc(sinAcentos(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := campos[:0]
	for _, t := range campos {
		if stopwords[t] || (len(t) < 2 && !unicode.IsDigit(rune(t[0]))) {
			continue
		}
		out = append(out, raiz(t))
	}
	return out
}

type Sparse struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

func hashTok(t string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(t))
	return h.Sum32()
}

const bmK1, bmB = 1.2, 0.75

func bm25Doc(tokens []string, avgdl float64) Sparse {
	if avgdl <= 0 {
		avgdl = 1
	}
	tf := map[uint32]float64{}
	for _, t := range tokens {
		tf[hashTok(t)]++
	}
	dl := float64(len(tokens))
	return armarSparse(tf, func(f float64) float64 {
		return f * (bmK1 + 1) / (f + bmK1*(1-bmB+bmB*dl/avgdl))
	})
}

func bm25Query(tokens []string) Sparse {
	tf := map[uint32]float64{}
	for _, t := range tokens {
		tf[hashTok(t)] = 1
	}
	return armarSparse(tf, func(float64) float64 { return 1 })
}

func armarSparse(tf map[uint32]float64, peso func(float64) float64) Sparse {
	idx := make([]uint32, 0, len(tf))
	for k := range tf {
		idx = append(idx, k)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	vals := make([]float32, len(idx))
	for i, k := range idx {
		vals[i] = float32(peso(tf[k]))
	}
	return Sparse{Indices: idx, Values: vals}
}
