package llm

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// HashEmbedder is a deterministic bag-of-words embedder (feature hashing of
// words and word pairs). It is much weaker than a real model, but it needs no
// network, which makes it useful for tests and for OpenAI-compatible
// providers that have no embeddings endpoint.
type HashEmbedder struct {
	Dim int
}

func (h HashEmbedder) Model() string { return "hash-256" }

func (h HashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := h.Dim
	if dim == 0 {
		dim = 256
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashVector(t, dim)
	}
	return out, nil
}

func hashVector(text string, dim int) []float32 {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.'
	})
	v := make([]float32, dim)
	add := func(tok string) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum32()
		sign := float32(1)
		if sum&1 == 1 {
			sign = -1
		}
		v[int(sum>>1)%dim] += sign
	}
	for i, w := range words {
		add(w)
		if i > 0 {
			add(words[i-1] + " " + w)
		}
	}

	var norm float64
	for _, x := range v {
		norm += float64(x * x)
	}
	if norm == 0 {
		return v
	}
	n := float32(math.Sqrt(norm))
	for i := range v {
		v[i] /= n
	}
	return v
}
