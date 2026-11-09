package store

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/j0sehernan/dispute-copilot/internal/rules"
)

// The methods below implement rules.VectorStore.

func (s *Store) ChunkHashes(ctx context.Context, model string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, content_hash FROM rule_chunks WHERE embed_model = $1`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		out[id] = hash
	}
	return out, rows.Err()
}

func (s *Store) UpsertChunks(ctx context.Context, model string, chunks []rules.Chunk, vectors [][]float32) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for i, c := range chunks {
			_, err := tx.Exec(ctx, `
				INSERT INTO rule_chunks (id, embed_model, doc_id, network, heading, content, content_hash, embedding)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::vector)
				ON CONFLICT (id, embed_model) DO UPDATE SET
					doc_id = EXCLUDED.doc_id, network = EXCLUDED.network, heading = EXCLUDED.heading,
					content = EXCLUDED.content, content_hash = EXCLUDED.content_hash, embedding = EXCLUDED.embedding`,
				c.ID, model, c.DocID, c.Network, c.Heading, c.Content, c.Hash(), vectorLiteral(vectors[i]))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteChunks(ctx context.Context, model string, keep []string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM rule_chunks WHERE embed_model = $1 AND NOT (id = ANY($2))`, model, keep)
	return err
}

func (s *Store) SearchChunks(ctx context.Context, model string, vector []float32, networks []string, limit int) ([]rules.ScoredChunk, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, doc_id, network, heading, content, 1 - (embedding <=> $1::vector) AS score
		FROM rule_chunks
		WHERE embed_model = $2 AND network = ANY($3)
		ORDER BY embedding <=> $1::vector
		LIMIT $4`,
		vectorLiteral(vector), model, networks, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rules.ScoredChunk
	for rows.Next() {
		var c rules.ScoredChunk
		if err := rows.Scan(&c.ID, &c.DocID, &c.Network, &c.Heading, &c.Content, &c.Score); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// vectorLiteral formats a vector in pgvector's text format: [1,2,3].
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
