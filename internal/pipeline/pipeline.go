// Package pipeline runs a dispute through its steps. Each step is a job:
// it loads the dispute, does its work, and saves the result together with
// the next job. Steps are idempotent so the queue can retry them.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/agent"
	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/queue"
	"github.com/j0sehernan/dispute-copilot/internal/rules"
)

const (
	JobExtract = "extract"
	JobGather  = "gather_evidence"
	JobAnalyze = "analyze"
)

type Store interface {
	GetDispute(ctx context.Context, id uuid.UUID) (*domain.Dispute, error)
	StartStep(ctx context.Context, id uuid.UUID, from, to domain.Status) error
	SaveExtraction(ctx context.Context, id uuid.UUID, det domain.Details, cat domain.Category, ruleID, nextJob string) error
	ListEvidence(ctx context.Context, id uuid.UUID) (domain.EvidenceSet, error)
	SaveEvidence(ctx context.Context, id uuid.UUID, items []domain.EvidenceItem, nextJob string) error
	SaveAnalysis(ctx context.Context, id uuid.UUID, cat domain.Category, a domain.Analysis, letter domain.Letter) error
	MarkFailed(ctx context.Context, id uuid.UUID, reason string) error
}

type (
	Extractor interface {
		Extract(ctx context.Context, id uuid.UUID, notice string) (domain.Details, error)
	}
	Gatherer interface {
		Gather(ctx context.Context, id uuid.UUID, d domain.Details) (agent.GatherResult, error)
	}
	Retriever interface {
		Retrieve(ctx context.Context, q rules.Query) (rules.Result, error)
	}
	Analyst interface {
		Analyze(ctx context.Context, in agent.AnalysisInput) (domain.Analysis, error)
	}
	Drafter interface {
		Draft(ctx context.Context, id uuid.UUID, d domain.Details, a domain.Analysis, ev domain.EvidenceSet) (domain.Letter, error)
	}
	RuleBook interface {
		ByReasonCode(network domain.Network, code string) (*rules.Doc, bool)
	}
)

type Pipeline struct {
	Store     Store
	Recorder  agent.Recorder
	Rules     RuleBook
	Extractor Extractor
	Gatherer  Gatherer
	Retriever Retriever
	Analyst   Analyst
	Drafter   Drafter
	Log       *slog.Logger
}

func (p *Pipeline) Handlers() map[string]queue.Handler {
	return map[string]queue.Handler{
		JobExtract: p.extract,
		JobGather:  p.gather,
		JobAnalyze: p.analyze,
	}
}

// OnFailure marks the dispute failed once its job has no retries left.
func (p *Pipeline) OnFailure(ctx context.Context, job queue.Job, err error) {
	reason := fmt.Sprintf("%s step failed: %v", job.Kind, err)
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if merr := p.Store.MarkFailed(ctx, job.DisputeID, reason); merr != nil {
		p.Log.Error("mark failed", "dispute_id", job.DisputeID, "err", merr)
	}
}

func (p *Pipeline) extract(ctx context.Context, job queue.Job) error {
	d, err := p.Store.GetDispute(ctx, job.DisputeID)
	if err != nil {
		return classify(err)
	}
	if skip, err := p.ready(d, domain.StatusReceived, domain.StatusExtracting); skip || err != nil {
		return err
	}
	if err := p.Store.StartStep(ctx, d.ID, domain.StatusReceived, domain.StatusExtracting); err != nil {
		return classify(err)
	}

	var det domain.Details
	if d.InputKind == domain.InputJSON {
		det, err = intake.ParseWebhook([]byte(d.RawInput))
		p.Recorder.Record(ctx, domain.AuditEvent{
			DisputeID: d.ID, Step: "extract", Kind: domain.AuditParse, Name: "webhook_payload", OK: err == nil,
		})
	} else {
		det, err = p.Extractor.Extract(ctx, d.ID, d.RawInput)
	}
	if err != nil {
		return classify(err)
	}

	var cat domain.Category
	var ruleID string
	if doc, ok := p.Rules.ByReasonCode(det.Network, det.ReasonCode); ok {
		cat, ruleID = doc.Category, doc.ID
	}
	return classify(p.Store.SaveExtraction(ctx, d.ID, det, cat, ruleID, JobGather))
}

func (p *Pipeline) gather(ctx context.Context, job queue.Job) error {
	d, err := p.Store.GetDispute(ctx, job.DisputeID)
	if err != nil {
		return classify(err)
	}
	if skip, err := p.ready(d, domain.StatusGatheringEvidence); skip || err != nil {
		return err
	}

	res, err := p.Gatherer.Gather(ctx, d.ID, *d.Details)
	if err != nil {
		return classify(err)
	}
	if len(res.Backstop) > 0 {
		p.Log.Info("evidence backstop used", "dispute_id", d.ID, "lookups", res.Backstop)
	}

	items := make([]domain.EvidenceItem, 0, len(res.Evidence))
	for _, src := range domain.EvidenceSources {
		if it, ok := res.Evidence[src]; ok {
			items = append(items, it)
		}
	}
	return classify(p.Store.SaveEvidence(ctx, d.ID, items, JobAnalyze))
}

func (p *Pipeline) analyze(ctx context.Context, job queue.Job) error {
	d, err := p.Store.GetDispute(ctx, job.DisputeID)
	if err != nil {
		return classify(err)
	}
	if skip, err := p.ready(d, domain.StatusAnalyzing); skip || err != nil {
		return err
	}

	ev, err := p.Store.ListEvidence(ctx, d.ID)
	if err != nil {
		return classify(err)
	}
	det := *d.Details
	retrieved, err := p.Retriever.Retrieve(ctx, rules.Query{
		Network:    det.Network,
		ReasonCode: det.ReasonCode,
		Claim:      det.CardholderClaim,
	})
	if err != nil {
		return classify(err)
	}

	analysis, err := p.Analyst.Analyze(ctx, agent.AnalysisInput{
		DisputeID: d.ID,
		Details:   det,
		Evidence:  ev,
		Rules:     retrieved,
	})
	if err != nil {
		return classify(err)
	}

	// Accepting a chargeback needs no letter; the reviewer can still write
	// one if they decide to fight.
	var letter domain.Letter
	if analysis.Recommendation == domain.RecommendFight {
		letter, err = p.Drafter.Draft(ctx, d.ID, det, analysis, ev)
		if err != nil {
			return classify(err)
		}
	}
	return classify(p.Store.SaveAnalysis(ctx, d.ID, retrieved.Rule.Category, analysis, letter))
}

// ready checks that the dispute is at this step. Any other status means the
// job is a duplicate delivery or the dispute failed: there is nothing to do.
func (p *Pipeline) ready(d *domain.Dispute, accepted ...domain.Status) (skip bool, err error) {
	if !slices.Contains(accepted, d.Status) {
		p.Log.Info("skipping job, dispute is not at this step", "dispute_id", d.ID, "status", d.Status)
		return true, nil
	}
	needsDetails := d.Status != domain.StatusReceived && d.Status != domain.StatusExtracting
	if needsDetails && d.Details == nil {
		return false, queue.Permanent(errors.New("dispute has no extracted details"))
	}
	return false, nil
}

// classify decides whether the queue should retry an error.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, llm.ErrTemporary), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, agent.ErrInvalidOutput),
		errors.Is(err, domain.ErrInvalidInput),
		errors.Is(err, domain.ErrInvalidState),
		errors.Is(err, domain.ErrNotFound):
		return queue.Permanent(err)
	}
	var se *llm.StatusError
	if errors.As(err, &se) {
		// 4xx other than 429: bad key, unknown model, bad request.
		return queue.Permanent(err)
	}
	return err
}
