package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/rules"
)

type analysisOutput struct {
	Requirements   []domain.RequirementResult `json:"requirements"`
	Recommendation domain.Recommendation      `json:"recommendation"`
	Reasons        []domain.Reason            `json:"reasons"`
	Missing        []string                   `json:"missing_evidence"`
}

// Analyst compares the evidence with the rule and recommends fight or
// accept.
type Analyst struct {
	client   llm.Client
	recorder Recorder
	prompt   Prompt
	schema   schema
}

func NewAnalyst(client llm.Client, rec Recorder) *Analyst {
	return &Analyst{
		client:   client,
		recorder: rec,
		prompt:   loadPrompt("analyze.v1"),
		schema:   loadSchema("analysis"),
	}
}

func (a *Analyst) PromptVersion() string { return a.prompt.Version }

type AnalysisInput struct {
	DisputeID uuid.UUID
	Details   domain.Details
	Evidence  domain.EvidenceSet
	Rules     rules.Result
}

func (a *Analyst) Analyze(ctx context.Context, in AnalysisInput) (domain.Analysis, error) {
	rule := in.Rules.Rule
	call := structured[analysisOutput]{
		client:   a.client,
		recorder: a.recorder,
		prompt:   a.prompt,
		schema:   a.schema,
		step:     "analyze",
		check: func(out analysisOutput) error {
			return checkAnalysis(out, rule.Requirements, in.Evidence, in.Rules.ChunkIDs())
		},
	}

	out, err := call.run(ctx, in.DisputeID, analysisPrompt(in))
	if err != nil {
		return domain.Analysis{}, err
	}

	// Keep the rule's wording of each requirement, not the model's.
	text := map[string]string{}
	for _, r := range rule.Requirements {
		text[r.ID] = r.Text
	}
	for i := range out.Requirements {
		out.Requirements[i].Text = text[out.Requirements[i].ID]
	}
	slices.SortStableFunc(out.Requirements, func(x, y domain.RequirementResult) int {
		return indexOf(rule.Requirements, x.ID) - indexOf(rule.Requirements, y.ID)
	})

	analysis := domain.Analysis{
		RuleID:              rule.ID,
		RuleMatch:           in.Rules.Match,
		Requirements:        out.Requirements,
		ModelRecommendation: out.Recommendation,
		Recommendation:      rule.Policy().Decide(out.Requirements),
		Reasons:             out.Reasons,
		MissingEvidence:     mergeMissing(out.Missing, in.Evidence),
		RuleChunks:          in.Rules.ChunkIDs(),
	}
	// The policy is the source of truth. If the model's own assessment of
	// the requirements implies the other answer, we follow the policy and
	// flag it so the reviewer looks twice.
	if analysis.Recommendation != analysis.ModelRecommendation {
		analysis.PolicyOverride = true
		a.recorder.Record(ctx, domain.AuditEvent{
			DisputeID: in.DisputeID,
			Step:      "analyze",
			Kind:      domain.AuditGuardrail,
			Name:      "policy_override",
			OK:        true,
			Detail: map[string]any{
				"model":  string(analysis.ModelRecommendation),
				"policy": string(analysis.Recommendation),
			},
		})
	}
	return analysis, nil
}

func indexOf(reqs []rules.Requirement, id string) int {
	for i, r := range reqs {
		if r.ID == id {
			return i
		}
	}
	return len(reqs)
}

// checkAnalysis is the anti-hallucination check. Every requirement must be
// answered once, "met" needs proof, and every citation must point to a field
// we actually fetched or to a rule section we actually retrieved.
func checkAnalysis(out analysisOutput, reqs []rules.Requirement, ev domain.EvidenceSet, chunkIDs []string) error {
	var errs []error

	want := map[string]bool{}
	for _, r := range reqs {
		want[r.ID] = true
	}
	seen := map[string]bool{}
	for _, r := range out.Requirements {
		switch {
		case !want[r.ID]:
			errs = append(errs, fmt.Errorf("requirement %q is not in the checklist", r.ID))
		case seen[r.ID]:
			errs = append(errs, fmt.Errorf("requirement %q appears twice", r.ID))
		}
		seen[r.ID] = true
		if r.Status != domain.RequirementMissingEvidence && len(r.Evidence) == 0 {
			errs = append(errs, fmt.Errorf("requirement %q is %s but cites no evidence", r.ID, r.Status))
		}
		errs = append(errs, checkPaths(ev, r.Evidence)...)
	}
	for _, r := range reqs {
		if !seen[r.ID] {
			errs = append(errs, fmt.Errorf("requirement %q is missing from the answer", r.ID))
		}
	}

	known := map[string]bool{}
	for _, id := range chunkIDs {
		known[id] = true
	}
	for i, reason := range out.Reasons {
		if len(reason.Evidence) == 0 && len(reason.Rules) == 0 {
			errs = append(errs, fmt.Errorf("reason %d cites nothing", i+1))
		}
		errs = append(errs, checkPaths(ev, reason.Evidence)...)
		for _, id := range reason.Rules {
			if !known[id] {
				errs = append(errs, fmt.Errorf("rule section %q was not provided", id))
			}
		}
	}
	return errors.Join(errs...)
}

func checkPaths(ev domain.EvidenceSet, paths []string) []error {
	var errs []error
	for _, p := range paths {
		if strings.Count(p, ".") == 0 {
			errs = append(errs, fmt.Errorf("evidence path %q must point to a field, like %s.<field>", p, p))
			continue
		}
		if _, ok := ev.Lookup(p); !ok {
			errs = append(errs, fmt.Errorf("evidence path %q does not exist in the records", p))
		}
	}
	return errs
}

// mergeMissing adds the records that were never found to what the model
// listed, so a missing record is always visible to the reviewer.
func mergeMissing(fromModel []string, ev domain.EvidenceSet) []string {
	out := slices.Clone(fromModel)
	for _, src := range ev.Missing() {
		note := string(src) + " record not available"
		if !slices.ContainsFunc(out, func(s string) bool { return strings.Contains(strings.ToLower(s), string(src)) }) {
			out = append(out, note)
		}
	}
	return out
}

func analysisPrompt(in AnalysisInput) string {
	var b strings.Builder
	d := in.Details

	fmt.Fprintf(&b, "## Dispute\nCase %s, %s reason code %s, amount %s.\nTransaction %s on %s. Opened %s.\nCardholder claim: %s\n\n",
		d.CaseID, d.Network, d.ReasonCode, d.Amount, d.TransactionID,
		d.TransactionDate.Format(time.DateOnly), d.OpenedAt.Format(time.DateOnly), d.CardholderClaim)

	rule := in.Rules.Rule
	fmt.Fprintf(&b, "## Requirement checklist (%s, %s)\n", rule.ID, rule.Title)
	for _, r := range rule.Requirements {
		fmt.Fprintf(&b, "- %s: %s\n", r.ID, r.Text)
	}
	b.WriteString("Conditions to fight (any one line, all ids on it met):\n")
	for _, g := range rule.FightWhen {
		fmt.Fprintf(&b, "- %s\n", strings.Join(g, " AND "))
	}

	b.WriteString("\n## Rule and guidance sections\n")
	b.WriteString(in.Rules.Render())

	b.WriteString("\n\n## Evidence records\n")
	for _, src := range domain.EvidenceSources {
		item, ok := in.Evidence[src]
		if !ok || !item.Found {
			fmt.Fprintf(&b, "### %s: NOT FOUND\n\n", src)
			continue
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", src, indentJSON(item.Data))
	}
	return b.String()
}

func indentJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
