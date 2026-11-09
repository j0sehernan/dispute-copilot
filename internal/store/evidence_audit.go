package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

func (s *Store) ListEvidence(ctx context.Context, id uuid.UUID) (domain.EvidenceSet, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, ref, found, data, fetched_at FROM evidence WHERE dispute_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	set := domain.EvidenceSet{}
	for rows.Next() {
		var it domain.EvidenceItem
		var data []byte
		if err := rows.Scan(&it.Source, &it.Ref, &it.Found, &data, &it.FetchedAt); err != nil {
			return nil, err
		}
		it.Data = data
		set[it.Source] = it
	}
	return set, rows.Err()
}

// Record implements agent.Recorder. A failed insert is logged and swallowed:
// losing an audit row is bad, failing the dispute because of it is worse.
func (s *Store) Record(ctx context.Context, ev domain.AuditEvent) {
	if ev.Detail == nil {
		ev.Detail = map[string]any{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_events
			(dispute_id, step, kind, name, prompt_version, model, input_tokens, output_tokens, latency_ms, ok, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		ev.DisputeID, ev.Step, ev.Kind, ev.Name, ev.PromptVersion, ev.Model,
		ev.InputTokens, ev.OutputTokens, ev.Latency.Milliseconds(), ev.OK, ev.Detail)
	if err != nil {
		s.log.Error("audit: insert failed", "dispute_id", ev.DisputeID, "step", ev.Step, "err", err)
	}
}

func (s *Store) ListAudit(ctx context.Context, id uuid.UUID) ([]domain.AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, dispute_id, step, kind, name, prompt_version, model,
		       input_tokens, output_tokens, latency_ms, ok, detail, created_at
		FROM audit_events WHERE dispute_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AuditEvent
	for rows.Next() {
		var ev domain.AuditEvent
		var ms int64
		if err := rows.Scan(&ev.ID, &ev.DisputeID, &ev.Step, &ev.Kind, &ev.Name, &ev.PromptVersion, &ev.Model,
			&ev.InputTokens, &ev.OutputTokens, &ms, &ev.OK, &ev.Detail, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Latency = time.Duration(ms) * time.Millisecond
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) Usage(ctx context.Context, id uuid.UUID) (domain.Usage, error) {
	var u domain.Usage
	var ms int64
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE kind = 'llm_call'),
			count(*) FILTER (WHERE kind = 'tool_call'),
			coalesce(sum(input_tokens), 0),
			coalesce(sum(output_tokens), 0),
			coalesce(sum(latency_ms) FILTER (WHERE kind = 'llm_call'), 0)
		FROM audit_events WHERE dispute_id = $1`, id,
	).Scan(&u.LLMCalls, &u.ToolCalls, &u.InputTokens, &u.OutputTokens, &ms)
	u.LLMLatency = time.Duration(ms) * time.Millisecond
	return u, err
}
