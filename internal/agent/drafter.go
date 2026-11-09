package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/redact"
)

type letterOutput struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Drafter writes the representment letter for disputes we fight.
type Drafter struct {
	client   llm.Client
	recorder Recorder
	prompt   Prompt
	schema   schema
}

func NewDrafter(client llm.Client, rec Recorder) *Drafter {
	return &Drafter{
		client:   client,
		recorder: rec,
		prompt:   loadPrompt("letter.v1"),
		schema:   loadSchema("letter"),
	}
}

func (d *Drafter) PromptVersion() string { return d.prompt.Version }

func (d *Drafter) Draft(ctx context.Context, disputeID uuid.UUID, det domain.Details, a domain.Analysis, ev domain.EvidenceSet) (domain.Letter, error) {
	call := structured[letterOutput]{
		client:   d.client,
		recorder: d.recorder,
		prompt:   d.prompt,
		schema:   d.schema,
		step:     "draft_letter",
		check: func(out letterOutput) error {
			return checkLetter(out, det)
		},
	}
	out, err := call.run(ctx, disputeID, letterPrompt(det, a, ev))
	if err != nil {
		return domain.Letter{}, err
	}
	return domain.Letter{Subject: strings.TrimSpace(out.Subject), Body: strings.TrimSpace(out.Body)}, nil
}

func checkLetter(out letterOutput, det domain.Details) error {
	var errs []error
	if !strings.Contains(out.Body, det.CaseID) {
		errs = append(errs, fmt.Errorf("the body must mention the case number %s", det.CaseID))
	}
	if redact.ContainsPAN(out.Body) || redact.ContainsPAN(out.Subject) {
		errs = append(errs, errors.New("the letter contains something that looks like a full card number; use only the last four digits"))
	}
	scrubbed := redact.String(out.Body)
	if strings.Contains(scrubbed, "[email]") {
		errs = append(errs, errors.New("the letter contains an email address; remove it"))
	}
	if strings.Contains(scrubbed, "[ip]") {
		errs = append(errs, errors.New("the letter contains an IP address; say \"the same IP address\" instead of the value"))
	}
	return errors.Join(errs...)
}

func letterPrompt(det domain.Details, a domain.Analysis, ev domain.EvidenceSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Dispute\nCase number: %s\nNetwork: %s\nReason code: %s\nAmount: %s\nTransaction: %s on %s\nCardholder claim: %s\n\n",
		det.CaseID, det.Network, det.ReasonCode, det.Amount, det.TransactionID,
		det.TransactionDate.Format(time.DateOnly), det.CardholderClaim)

	b.WriteString("## Requirements met, with the evidence values\n")
	for _, r := range a.Requirements {
		if r.Status != domain.RequirementMet {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", r.Text, r.Explanation)
		for _, p := range r.Evidence {
			if v, ok := ev.Lookup(p); ok {
				fmt.Fprintf(&b, "  - %s = %v\n", p, v)
			}
		}
	}

	b.WriteString("\n## Reasons\n")
	for _, r := range a.Reasons {
		fmt.Fprintf(&b, "- %s\n", r.Text)
	}
	if len(a.MissingEvidence) > 0 {
		b.WriteString("\n## Known gaps (do not claim these)\n")
		for _, m := range a.MissingEvidence {
			fmt.Fprintf(&b, "- %s\n", m)
		}
	}
	return b.String()
}
