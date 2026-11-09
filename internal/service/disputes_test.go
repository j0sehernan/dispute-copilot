package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

type fakeStore struct {
	byKey    map[string]uuid.UUID
	disputes map[uuid.UUID]*domain.Dispute
	jobs     []string
	saved    *domain.Review
	approved *domain.Review
}

func newFakeStore() *fakeStore {
	return &fakeStore{byKey: map[string]uuid.UUID{}, disputes: map[uuid.UUID]*domain.Dispute{}}
}

func (f *fakeStore) CreateDispute(_ context.Context, d *domain.Dispute, job string) (uuid.UUID, bool, error) {
	if id, ok := f.byKey[d.DedupeKey]; ok {
		return id, false, nil
	}
	f.byKey[d.DedupeKey] = d.ID
	d.Status = domain.StatusReceived
	f.disputes[d.ID] = d
	f.jobs = append(f.jobs, job)
	return d.ID, true, nil
}

func (f *fakeStore) GetDispute(_ context.Context, id uuid.UUID) (*domain.Dispute, error) {
	d, ok := f.disputes[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) ListDisputes(context.Context, int) ([]*domain.Dispute, error) { return nil, nil }
func (f *fakeStore) ListEvidence(context.Context, uuid.UUID) (domain.EvidenceSet, error) {
	return domain.EvidenceSet{}, nil
}
func (f *fakeStore) ListAudit(context.Context, uuid.UUID) ([]domain.AuditEvent, error) {
	return nil, nil
}
func (f *fakeStore) Usage(context.Context, uuid.UUID) (domain.Usage, error) {
	return domain.Usage{}, nil
}

func (f *fakeStore) SaveReview(_ context.Context, _ uuid.UUID, r domain.Review) error {
	f.saved = &r
	return nil
}

func (f *fakeStore) Approve(_ context.Context, _ uuid.UUID, r domain.Review) error {
	f.approved = &r
	return nil
}

const notice = `Case MC-CB-2026-118204, Mastercard reason code 4837, USD 312.45, transaction txn_Q7M2Z8.`

func TestSubmitDetectsKindAndDedupes(t *testing.T) {
	st := newFakeStore()
	svc := NewDisputes(st)
	ctx := context.Background()

	first, err := svc.Submit(ctx, Submission{Source: domain.SourceUpload, Content: []byte(notice)})
	if err != nil || !first.Created {
		t.Fatalf("first submit: %+v, %v", first, err)
	}
	if st.disputes[first.ID].InputKind != domain.InputText {
		t.Errorf("kind = %s", st.disputes[first.ID].InputKind)
	}

	again, err := svc.Submit(ctx, Submission{Source: domain.SourceUpload, Content: []byte(notice)})
	if err != nil || again.Created || again.ID != first.ID {
		t.Errorf("resubmit should return the same dispute: %+v, %v", again, err)
	}
	if len(st.jobs) != 1 || st.jobs[0] != "extract" {
		t.Errorf("jobs = %v", st.jobs)
	}

	withKey, _ := svc.Submit(ctx, Submission{Source: domain.SourceWebhook, Content: []byte(notice), IdempotencyKey: "retry-1"})
	if !withKey.Created {
		t.Error("a new idempotency key is a new delivery")
	}
}

func TestSubmitRejectsBadInput(t *testing.T) {
	svc := NewDisputes(newFakeStore())
	for name, body := range map[string]string{
		"empty":     "",
		"too short": "chargeback",
		"bad json":  `{"case_id":"X","network":"visa"}`,
		"fake pdf":  "%PDF-1.4 garbage",
	} {
		_, err := svc.Submit(context.Background(), Submission{Content: []byte(body)})
		if !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func readyDispute(st *fakeStore) uuid.UUID {
	id := uuid.New()
	st.disputes[id] = &domain.Dispute{
		ID:     id,
		Status: domain.StatusReadyForReview,
		Letter: domain.Letter{Subject: "Case X", Body: "Original letter"},
	}
	return id
}

func TestReviewMarksEdits(t *testing.T) {
	st := newFakeStore()
	svc := NewDisputes(st)
	id := readyDispute(st)

	err := svc.SaveDraft(context.Background(), id, ReviewInput{
		Reviewer: "Ana", Decision: "fight", Subject: "Case X", Body: "Original letter",
	})
	if err != nil || st.saved.Letter.Edited {
		t.Fatalf("unchanged letter marked as edited: %+v, %v", st.saved, err)
	}

	err = svc.SaveDraft(context.Background(), id, ReviewInput{
		Reviewer: "Ana", Decision: "fight", Subject: "Case X", Body: "Original letter, with a fix",
	})
	if err != nil || !st.saved.Letter.Edited {
		t.Errorf("edit not detected: %+v, %v", st.saved, err)
	}
}

func TestApproveRules(t *testing.T) {
	st := newFakeStore()
	svc := NewDisputes(st)
	id := readyDispute(st)
	ctx := context.Background()

	cases := map[string]ReviewInput{
		"no reviewer":     {Decision: "fight", Body: "x"},
		"bad decision":    {Reviewer: "Ana", Decision: "maybe", Body: "x"},
		"fight no letter": {Reviewer: "Ana", Decision: "fight"},
	}
	for name, in := range cases {
		if err := svc.Approve(ctx, id, in); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	if err := svc.Approve(ctx, id, ReviewInput{Reviewer: "Ana", Decision: "accept"}); err != nil {
		t.Fatalf("accept without letter should be allowed: %v", err)
	}
	if st.approved == nil || st.approved.Decision != domain.RecommendAccept {
		t.Errorf("approved = %+v", st.approved)
	}

	st.disputes[id].Status = domain.StatusAnalyzing
	if err := svc.Approve(ctx, id, ReviewInput{Reviewer: "Ana", Decision: "accept"}); !errors.Is(err, domain.ErrInvalidState) {
		t.Errorf("approve while analyzing: err = %v", err)
	}
}
