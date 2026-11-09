package rules

import (
	"context"
	"fmt"
	"strings"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

type ScoredChunk struct {
	Chunk
	Score float64
}

// VectorStore keeps chunk embeddings. Vectors from different embedding
// models are kept apart, so switching provider just re-indexes.
type VectorStore interface {
	ChunkHashes(ctx context.Context, model string) (map[string]string, error)
	UpsertChunks(ctx context.Context, model string, chunks []Chunk, vectors [][]float32) error
	DeleteChunks(ctx context.Context, model string, keep []string) error
	SearchChunks(ctx context.Context, model string, vector []float32, networks []string, limit int) ([]ScoredChunk, error)
}

type Retriever struct {
	lib      *Library
	store    VectorStore
	embedder llm.Embedder
	// Extra is how many similar chunks to add on top of the rule's own.
	Extra int
}

func NewRetriever(lib *Library, store VectorStore, embedder llm.Embedder) *Retriever {
	return &Retriever{lib: lib, store: store, embedder: embedder, Extra: 3}
}

// Index embeds chunks that are new or changed since the last run. It is
// cheap to call on every worker start.
func (r *Retriever) Index(ctx context.Context) (int, error) {
	model := r.embedder.Model()
	have, err := r.store.ChunkHashes(ctx, model)
	if err != nil {
		return 0, err
	}

	var (
		todo  []Chunk
		texts []string
		keep  []string
	)
	for _, doc := range r.lib.Docs() {
		for _, c := range doc.Chunks {
			keep = append(keep, c.ID)
			if have[c.ID] == c.Hash() {
				continue
			}
			todo = append(todo, c)
			texts = append(texts, c.EmbedText(doc.Title))
		}
	}
	if err := r.store.DeleteChunks(ctx, model, keep); err != nil {
		return 0, err
	}
	if len(todo) == 0 {
		return 0, nil
	}

	vecs, err := r.embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed rule chunks: %w", err)
	}
	if err := r.store.UpsertChunks(ctx, model, todo, vecs); err != nil {
		return 0, err
	}
	return len(todo), nil
}

type Query struct {
	Network    domain.Network
	ReasonCode string
	Claim      string
}

func (q Query) text() string {
	return fmt.Sprintf("%s reason code %s. Cardholder claim: %s", q.Network, q.ReasonCode, q.Claim)
}

const (
	MatchReasonCode = "reason_code"
	MatchSimilarity = "similarity"
)

type Result struct {
	Rule   *Doc
	Match  string
	Chunks []ScoredChunk
}

// ChunkIDs lists every chunk the analysis may cite.
func (r Result) ChunkIDs() []string {
	ids := make([]string, len(r.Chunks))
	for i, c := range r.Chunks {
		ids[i] = c.ID
	}
	return ids
}

// Retrieve picks the rule for the dispute and the supporting context.
//
// The rule itself is chosen by metadata (network + reason code) because its
// requirement checklist drives validation and must be exact. Vector search
// adds the closest guidance sections, and is the fallback when the reason
// code is not in the library.
func (r *Retriever) Retrieve(ctx context.Context, q Query) (Result, error) {
	vecs, err := r.embedder.Embed(ctx, []string{q.text()})
	if err != nil {
		return Result{}, fmt.Errorf("embed query: %w", err)
	}
	similar, err := r.store.SearchChunks(ctx, r.embedder.Model(), vecs[0], []string{string(q.Network), "any"}, 12)
	if err != nil {
		return Result{}, err
	}

	res := Result{Match: MatchReasonCode}
	if doc, ok := r.lib.ByReasonCode(q.Network, q.ReasonCode); ok {
		res.Rule = doc
	} else {
		for _, c := range similar {
			if doc, ok := r.lib.Doc(c.DocID); ok && doc.IsRule() {
				res.Rule, res.Match = doc, MatchSimilarity
				break
			}
		}
	}
	if res.Rule == nil {
		return Result{}, fmt.Errorf("no rule found for %s %s", q.Network, q.ReasonCode)
	}

	for _, c := range res.Rule.Chunks {
		res.Chunks = append(res.Chunks, ScoredChunk{Chunk: c, Score: 1})
	}
	added := 0
	for _, c := range similar {
		if added == r.Extra {
			break
		}
		doc, _ := r.lib.Doc(c.DocID)
		// Other rules' sections would only confuse the analysis.
		if c.DocID == res.Rule.ID || (doc != nil && doc.IsRule()) {
			continue
		}
		res.Chunks = append(res.Chunks, c)
		added++
	}
	return res, nil
}

// Render formats the result for a prompt.
func (r Result) Render() string {
	var b strings.Builder
	for _, c := range r.Chunks {
		fmt.Fprintf(&b, "[%s] %s\n%s\n\n", c.ID, c.Heading, c.Content)
	}
	return strings.TrimSpace(b.String())
}
