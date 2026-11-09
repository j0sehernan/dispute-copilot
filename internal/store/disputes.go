package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

const disputeColumns = `
	id, source, input_kind, raw_input, dedupe_key, status,
	case_id, network, reason_code, amount_minor, currency, transaction_id,
	transaction_date, opened_at, respond_by, cardholder_claim,
	category, rule_id, analysis, letter_subject, letter_body, letter_edited,
	decision, reviewed_by, failure_reason,
	created_at, updated_at, ready_at, approved_at`

// CreateDispute stores a new dispute and queues its first job in the same
// transaction. If a dispute with the same dedupe key exists, it returns that
// one's id and created=false.
func (s *Store) CreateDispute(ctx context.Context, d *domain.Dispute, firstJob string) (id uuid.UUID, created bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO disputes (id, source, input_kind, raw_input, dedupe_key, status)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (dedupe_key) DO NOTHING
			RETURNING id`,
			d.ID, d.Source, d.InputKind, d.RawInput, d.DedupeKey, domain.StatusReceived,
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT id FROM disputes WHERE dedupe_key = $1`, d.DedupeKey).Scan(&id)
		}
		if err != nil {
			return err
		}
		created = true
		if err := statusEvent(ctx, tx, id, "", domain.StatusReceived); err != nil {
			return err
		}
		return enqueue(ctx, tx, firstJob, id)
	})
	return id, created, err
}

func (s *Store) GetDispute(ctx context.Context, id uuid.UUID) (*domain.Dispute, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+disputeColumns+` FROM disputes WHERE id = $1`, id)
	d, err := scanDispute(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return d, err
}

func (s *Store) ListDisputes(ctx context.Context, limit int) ([]*domain.Dispute, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+disputeColumns+` FROM disputes ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StartStep moves the dispute into a working status. It accepts the target
// status as the current one too, so a job that is retried after a crash can
// start again.
func (s *Store) StartStep(ctx context.Context, id uuid.UUID, from, to domain.Status) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur == to {
			return nil
		}
		if cur != from {
			return fmt.Errorf("%w: dispute is %s, step needs %s", domain.ErrInvalidState, cur, from)
		}
		return setStatus(ctx, tx, id, cur, to, nil)
	})
}

// SaveExtraction stores the notice fields, moves the dispute to
// gathering_evidence and queues the next job.
func (s *Store) SaveExtraction(ctx context.Context, id uuid.UUID, det domain.Details, cat domain.Category, ruleID, nextJob string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := expectStatus(ctx, tx, id, domain.StatusExtracting); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE disputes SET
				case_id = $2, network = $3, reason_code = $4, amount_minor = $5, currency = $6,
				transaction_id = $7, transaction_date = $8, opened_at = $9, respond_by = $10,
				cardholder_claim = $11, category = $12, rule_id = $13
			WHERE id = $1`,
			id, det.CaseID, det.Network, det.ReasonCode, det.Amount.Minor, det.Amount.Currency,
			det.TransactionID, det.TransactionDate, nullDate(det.OpenedAt), det.RespondBy,
			det.CardholderClaim, cat, ruleID)
		if err != nil {
			return err
		}
		if err := setStatus(ctx, tx, id, domain.StatusExtracting, domain.StatusGatheringEvidence, nil); err != nil {
			return err
		}
		return enqueue(ctx, tx, nextJob, id)
	})
}

// SaveEvidence stores the evidence, moves the dispute to analyzing and
// queues the next job.
func (s *Store) SaveEvidence(ctx context.Context, id uuid.UUID, items []domain.EvidenceItem, nextJob string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := expectStatus(ctx, tx, id, domain.StatusGatheringEvidence); err != nil {
			return err
		}
		for _, it := range items {
			var data []byte
			if it.Found {
				data = it.Data
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO evidence (dispute_id, source, ref, found, data, fetched_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (dispute_id, source) DO UPDATE
				SET ref = EXCLUDED.ref, found = EXCLUDED.found, data = EXCLUDED.data, fetched_at = EXCLUDED.fetched_at`,
				id, it.Source, it.Ref, it.Found, data, it.FetchedAt)
			if err != nil {
				return err
			}
		}
		if err := setStatus(ctx, tx, id, domain.StatusGatheringEvidence, domain.StatusAnalyzing, nil); err != nil {
			return err
		}
		return enqueue(ctx, tx, nextJob, id)
	})
}

// SaveAnalysis stores the analysis and the draft letter and hands the
// dispute to a human.
func (s *Store) SaveAnalysis(ctx context.Context, id uuid.UUID, cat domain.Category, a domain.Analysis, letter domain.Letter) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := expectStatus(ctx, tx, id, domain.StatusAnalyzing); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE disputes SET analysis = $2, letter_subject = $3, letter_body = $4,
				letter_edited = false, decision = $5, ready_at = now(),
				rule_id = $6, category = coalesce(nullif(category, ''), $7)
			WHERE id = $1`,
			id, raw, letter.Subject, letter.Body, a.Recommendation, a.RuleID, cat)
		if err != nil {
			return err
		}
		if err := setStatus(ctx, tx, id, domain.StatusAnalyzing, domain.StatusReadyForReview, nil); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_events (dispute_id, step, kind, name, ok, detail)
			VALUES ($1, 'analyze', $2, $3, true, $4)`,
			id, domain.AuditDecision, string(a.Recommendation), map[string]any{
				"rule_id":         a.RuleID,
				"policy_override": a.PolicyOverride,
				"model":           a.ModelRecommendation,
			})
		return err
	})
}

func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if !cur.Processing() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE disputes SET failure_reason = $2 WHERE id = $1`, id, reason); err != nil {
			return err
		}
		return setStatus(ctx, tx, id, cur, domain.StatusFailed, map[string]any{"reason": reason})
	})
}

// SaveReview stores the reviewer's edits. It is allowed only while the
// dispute waits for review.
func (s *Store) SaveReview(ctx context.Context, id uuid.UUID, r domain.Review) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := expectStatus(ctx, tx, id, domain.StatusReadyForReview); err != nil {
			return err
		}
		return saveReview(ctx, tx, id, r, "save_draft", domain.AuditHumanEdit)
	})
}

// Approve stores the final version and closes the review in one step.
func (s *Store) Approve(ctx context.Context, id uuid.UUID, r domain.Review) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := expectStatus(ctx, tx, id, domain.StatusReadyForReview); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE disputes SET reviewed_by = $2, approved_at = now() WHERE id = $1`, id, r.Reviewer); err != nil {
			return err
		}
		if err := saveReview(ctx, tx, id, r, string(r.Decision), domain.AuditHumanApprove); err != nil {
			return err
		}
		return setStatus(ctx, tx, id, domain.StatusReadyForReview, domain.StatusApproved, nil)
	})
}

func saveReview(ctx context.Context, tx pgx.Tx, id uuid.UUID, r domain.Review, name string, kind domain.AuditKind) error {
	_, err := tx.Exec(ctx, `
		UPDATE disputes SET letter_subject = $2, letter_body = $3, letter_edited = $4, decision = $5, updated_at = now()
		WHERE id = $1`,
		id, r.Letter.Subject, r.Letter.Body, r.Letter.Edited, r.Decision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (dispute_id, step, kind, name, ok, detail)
		VALUES ($1, 'review', $2, $3, true, $4)`,
		id, kind, name, map[string]any{
			"reviewer": r.Reviewer, "decision": r.Decision, "letter_edited": r.Letter.Edited,
		})
	return err
}

func lockStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.Status, error) {
	var cur domain.Status
	err := tx.QueryRow(ctx, `SELECT status FROM disputes WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	return cur, err
}

func expectStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, want domain.Status) error {
	cur, err := lockStatus(ctx, tx, id)
	if err != nil {
		return err
	}
	if cur != want {
		return fmt.Errorf("%w: dispute is %s, expected %s", domain.ErrInvalidState, cur, want)
	}
	return nil
}

func setStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to domain.Status, detail map[string]any) error {
	if _, err := tx.Exec(ctx, `UPDATE disputes SET status = $2, updated_at = now() WHERE id = $1`, id, to); err != nil {
		return err
	}
	return statusEventDetail(ctx, tx, id, from, to, detail)
}

func statusEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to domain.Status) error {
	return statusEventDetail(ctx, tx, id, from, to, nil)
}

func statusEventDetail(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to domain.Status, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from"] = from
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (dispute_id, step, kind, name, ok, detail)
		VALUES ($1, 'status', $2, $3, true, $4)`,
		id, domain.AuditStatus, string(to), detail)
	return err
}

func nullDate(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func scanDispute(row pgx.Row) (*domain.Dispute, error) {
	var (
		d                                                  domain.Dispute
		caseID, network, reasonCode, currency, txID, claim *string
		category, ruleID, decision, reviewedBy, failure    *string
		amount                                             *int64
		txDate, openedAt, respondBy                        *time.Time
		analysis                                           []byte
	)
	err := row.Scan(
		&d.ID, &d.Source, &d.InputKind, &d.RawInput, &d.DedupeKey, &d.Status,
		&caseID, &network, &reasonCode, &amount, &currency, &txID,
		&txDate, &openedAt, &respondBy, &claim,
		&category, &ruleID, &analysis, &d.Letter.Subject, &d.Letter.Body, &d.Letter.Edited,
		&decision, &reviewedBy, &failure,
		&d.CreatedAt, &d.UpdatedAt, &d.ReadyAt, &d.ApprovedAt,
	)
	if err != nil {
		return nil, err
	}

	if caseID != nil {
		det := domain.Details{
			CaseID:          *caseID,
			Network:         domain.Network(deref(network)),
			ReasonCode:      deref(reasonCode),
			TransactionID:   deref(txID),
			CardholderClaim: deref(claim),
			RespondBy:       respondBy,
		}
		if amount != nil {
			det.Amount = domain.Money{Minor: *amount, Currency: deref(currency)}
		}
		if txDate != nil {
			det.TransactionDate = *txDate
		}
		if openedAt != nil {
			det.OpenedAt = *openedAt
		}
		d.Details = &det
	}
	d.Category = domain.Category(deref(category))
	d.RuleID = deref(ruleID)
	d.Decision = domain.Recommendation(deref(decision))
	d.ReviewedBy = deref(reviewedBy)
	d.FailureReason = deref(failure)
	if len(analysis) > 0 {
		var a domain.Analysis
		if err := json.Unmarshal(analysis, &a); err != nil {
			return nil, fmt.Errorf("decode analysis: %w", err)
		}
		d.Analysis = &a
	}
	return &d, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
