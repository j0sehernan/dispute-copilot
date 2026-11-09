// Package service holds the use cases behind the HTTP layer: submitting a
// dispute, reading it, and the human review.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
	"github.com/j0sehernan/dispute-copilot/internal/pipeline"
)

type Store interface {
	CreateDispute(ctx context.Context, d *domain.Dispute, firstJob string) (uuid.UUID, bool, error)
	GetDispute(ctx context.Context, id uuid.UUID) (*domain.Dispute, error)
	ListDisputes(ctx context.Context, limit int) ([]*domain.Dispute, error)
	ListEvidence(ctx context.Context, id uuid.UUID) (domain.EvidenceSet, error)
	ListAudit(ctx context.Context, id uuid.UUID) ([]domain.AuditEvent, error)
	Usage(ctx context.Context, id uuid.UUID) (domain.Usage, error)
	SaveReview(ctx context.Context, id uuid.UUID, r domain.Review) error
	Approve(ctx context.Context, id uuid.UUID, r domain.Review) error
}

type Disputes struct {
	store Store
}

func NewDisputes(store Store) *Disputes {
	return &Disputes{store: store}
}

type Submission struct {
	Source         domain.Source
	Content        []byte
	IdempotencyKey string
}

type SubmitResult struct {
	ID      uuid.UUID
	Created bool
}

// Submit validates the input and queues the dispute for analysis. It only
// does cheap checks here; everything slow happens in the worker.
func (s *Disputes) Submit(ctx context.Context, sub Submission) (SubmitResult, error) {
	if len(sub.Content) == 0 {
		return SubmitResult{}, fmt.Errorf("%w: empty body", domain.ErrInvalidInput)
	}
	if len(sub.Content) > intake.MaxUploadBytes {
		return SubmitResult{}, fmt.Errorf("%w: file is larger than 5 MB", domain.ErrInvalidInput)
	}

	kind := intake.Detect(sub.Content)
	var raw string
	switch kind {
	case domain.InputJSON:
		if _, err := intake.ParseWebhook(sub.Content); err != nil {
			return SubmitResult{}, err
		}
		raw = string(sub.Content)
	case domain.InputPDF:
		text, err := intake.PDFText(sub.Content)
		if err != nil {
			return SubmitResult{}, err
		}
		raw = text
	default:
		text, err := intake.CleanText(sub.Content)
		if err != nil {
			return SubmitResult{}, err
		}
		raw = text
	}

	d := &domain.Dispute{
		ID:        uuid.New(),
		Source:    sub.Source,
		InputKind: kind,
		RawInput:  raw,
		DedupeKey: intake.DedupeKey(sub.IdempotencyKey, sub.Content),
	}
	id, created, err := s.store.CreateDispute(ctx, d, pipeline.JobExtract)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{ID: id, Created: created}, nil
}

// View is everything the review screen and the export need.
type View struct {
	Dispute  *domain.Dispute
	Evidence domain.EvidenceSet
	Audit    []domain.AuditEvent
	Usage    domain.Usage
}

func (s *Disputes) Get(ctx context.Context, id uuid.UUID) (*View, error) {
	d, err := s.store.GetDispute(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &View{Dispute: d}
	if v.Evidence, err = s.store.ListEvidence(ctx, id); err != nil {
		return nil, err
	}
	if v.Audit, err = s.store.ListAudit(ctx, id); err != nil {
		return nil, err
	}
	if v.Usage, err = s.store.Usage(ctx, id); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Disputes) List(ctx context.Context) ([]*domain.Dispute, error) {
	return s.store.ListDisputes(ctx, 200)
}

type ReviewInput struct {
	Reviewer string
	Decision string
	Subject  string
	Body     string
}

// SaveDraft keeps the reviewer's changes without closing the review.
func (s *Disputes) SaveDraft(ctx context.Context, id uuid.UUID, in ReviewInput) error {
	r, err := s.review(ctx, id, in, false)
	if err != nil {
		return err
	}
	return s.store.SaveReview(ctx, id, r)
}

// Approve closes the review. Nothing is sent anywhere: the reviewer
// downloads the package and submits it through their acquirer.
func (s *Disputes) Approve(ctx context.Context, id uuid.UUID, in ReviewInput) error {
	r, err := s.review(ctx, id, in, true)
	if err != nil {
		return err
	}
	return s.store.Approve(ctx, id, r)
}

func (s *Disputes) review(ctx context.Context, id uuid.UUID, in ReviewInput, final bool) (domain.Review, error) {
	d, err := s.store.GetDispute(ctx, id)
	if err != nil {
		return domain.Review{}, err
	}
	if d.Status != domain.StatusReadyForReview {
		return domain.Review{}, fmt.Errorf("%w: dispute is %s", domain.ErrInvalidState, d.Status)
	}

	r := domain.Review{
		Reviewer: strings.TrimSpace(in.Reviewer),
		Decision: domain.Recommendation(in.Decision),
		Letter: domain.Letter{
			Subject: strings.TrimSpace(in.Subject),
			Body:    strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n")),
		},
	}
	r.Letter.Edited = d.Letter.Edited || r.Letter.Subject != d.Letter.Subject || r.Letter.Body != d.Letter.Body

	var errs []error
	if r.Reviewer == "" || len(r.Reviewer) > 100 {
		errs = append(errs, errors.New("reviewer name is required (max 100 characters)"))
	}
	if !r.Decision.Valid() {
		errs = append(errs, errors.New("decision must be fight or accept"))
	}
	if len(r.Letter.Body) > 20_000 {
		errs = append(errs, errors.New("letter is too long"))
	}
	if final && r.Decision == domain.RecommendFight && r.Letter.Body == "" {
		errs = append(errs, errors.New("a response letter is required to fight the dispute"))
	}
	if err := errors.Join(errs...); err != nil {
		return domain.Review{}, fmt.Errorf("%w: %w", domain.ErrInvalidInput, err)
	}
	return r, nil
}
