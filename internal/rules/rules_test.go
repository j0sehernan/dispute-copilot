package rules

import (
	"context"
	"math"
	"sort"
	"testing"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

func TestLibraryLoads(t *testing.T) {
	lib, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		network domain.Network
		code    string
		cat     domain.Category
	}{
		{domain.NetworkVisa, "10.4", domain.CategoryFraud},
		{domain.NetworkVisa, "13.1", domain.CategoryNotReceived},
		{domain.NetworkVisa, "13.3", domain.CategoryNotAsDescribed},
		{domain.NetworkVisa, "12.6.1", domain.CategoryDuplicate},
		{domain.NetworkVisa, "13.6", domain.CategoryCreditNotProcessed},
		{domain.NetworkMastercard, "4837", domain.CategoryFraud},
		{domain.NetworkMastercard, "4855", domain.CategoryNotReceived},
		{domain.NetworkMastercard, "4853", domain.CategoryNotAsDescribed},
		{domain.NetworkMastercard, "4834", domain.CategoryDuplicate},
		{domain.NetworkMastercard, "4860", domain.CategoryCreditNotProcessed},
	}
	for _, c := range cases {
		doc, ok := lib.ByReasonCode(c.network, c.code)
		if !ok {
			t.Errorf("%s %s not found", c.network, c.code)
			continue
		}
		if doc.Category != c.cat {
			t.Errorf("%s: category %s, want %s", doc.ID, doc.Category, c.cat)
		}
		if len(doc.Chunks) < 2 {
			t.Errorf("%s: only %d chunks", doc.ID, len(doc.Chunks))
		}
	}
	if _, ok := lib.ByReasonCode(domain.NetworkVisa, "4837"); ok {
		t.Error("reason codes must be scoped by network")
	}
}

func TestParseRejectsBadPolicy(t *testing.T) {
	raw := []byte("---\nid: x\nnetwork: visa\nreason_code: \"1\"\ncategory: fraud\ntitle: X\nrequirements:\n  - id: a\n    text: A\nfight_when:\n  - [b]\n---\n## S\ntext\n")
	if _, err := parse(raw); err == nil {
		t.Fatal("expected error for unknown requirement in fight_when")
	}
}

func TestSlug(t *testing.T) {
	if got := slug("What does not win"); got != "what-does-not-win" {
		t.Errorf("slug = %q", got)
	}
}

// memStore is an in-memory VectorStore with brute-force cosine search.
type memStore struct {
	chunks map[string]Chunk
	vecs   map[string][]float32
	hashes map[string]string
}

func newMemStore() *memStore {
	return &memStore{chunks: map[string]Chunk{}, vecs: map[string][]float32{}, hashes: map[string]string{}}
}

func (m *memStore) ChunkHashes(context.Context, string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.hashes {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) UpsertChunks(_ context.Context, _ string, cs []Chunk, vs [][]float32) error {
	for i, c := range cs {
		m.chunks[c.ID], m.vecs[c.ID], m.hashes[c.ID] = c, vs[i], c.Hash()
	}
	return nil
}

func (m *memStore) DeleteChunks(context.Context, string, []string) error { return nil }

func (m *memStore) SearchChunks(_ context.Context, _ string, v []float32, networks []string, limit int) ([]ScoredChunk, error) {
	allowed := map[string]bool{}
	for _, n := range networks {
		allowed[n] = true
	}
	var out []ScoredChunk
	for id, c := range m.chunks {
		if allowed[c.Network] {
			out = append(out, ScoredChunk{Chunk: c, Score: cosine(v, m.vecs[id])})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	return dot / (math.Sqrt(na)*math.Sqrt(nb) + 1e-9)
}

func TestRetrieve(t *testing.T) {
	lib, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	r := NewRetriever(lib, store, llm.HashEmbedder{})
	ctx := context.Background()

	n, err := r.Index(ctx)
	if err != nil || n == 0 {
		t.Fatalf("Index = %d, %v", n, err)
	}
	if n2, _ := r.Index(ctx); n2 != 0 {
		t.Errorf("second Index embedded %d chunks, want 0", n2)
	}

	res, err := r.Retrieve(ctx, Query{Network: domain.NetworkVisa, ReasonCode: "13.1", Claim: "package never arrived"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rule.ID != "visa-13.1" || res.Match != MatchReasonCode {
		t.Errorf("rule = %s (%s)", res.Rule.ID, res.Match)
	}
	for _, c := range res.Chunks {
		if doc, _ := lib.Doc(c.DocID); doc.IsRule() && c.DocID != "visa-13.1" {
			t.Errorf("chunk from another rule leaked in: %s", c.ID)
		}
		if c.Network == "mastercard" {
			t.Errorf("mastercard chunk for a visa dispute: %s", c.ID)
		}
	}

	// Unknown code falls back to the nearest rule.
	res, err = r.Retrieve(ctx, Query{Network: domain.NetworkMastercard, ReasonCode: "9999", Claim: "goods or services not provided, delivered tracking"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Match != MatchSimilarity || !res.Rule.IsRule() || res.Rule.Network != "mastercard" {
		t.Errorf("fallback rule = %s (%s)", res.Rule.ID, res.Match)
	}
}
