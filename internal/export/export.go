// Package export renders the evidence package a reviewer downloads, as
// Markdown or PDF.
package export

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

type Package struct {
	Dispute  *domain.Dispute
	Evidence domain.EvidenceSet
	Audit    []domain.AuditEvent
}

// FileName is a safe file name for the download.
func (p Package) FileName(ext string) string {
	name := p.Dispute.ID.String()[:8]
	if p.Dispute.Details != nil {
		name = p.Dispute.Details.CaseID
	}
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	return "dispute-" + name + "." + ext
}

func (p Package) statusLine() string {
	d := p.Dispute
	if d.Status == domain.StatusApproved && d.ApprovedAt != nil {
		return fmt.Sprintf("Approved by %s on %s", d.ReviewedBy, d.ApprovedAt.UTC().Format(time.DateOnly))
	}
	return "DRAFT - not approved yet"
}

type field struct{ label, value string }

func (p Package) fields() []field {
	d := p.Dispute
	if d.Details == nil {
		return nil
	}
	det := d.Details
	due := "-"
	if det.RespondBy != nil {
		due = det.RespondBy.Format(time.DateOnly)
	}
	return []field{
		{"Case number", det.CaseID},
		{"Network", networkName(det.Network)},
		{"Reason code", det.ReasonCode + " (" + categoryName(d.Category) + ")"},
		{"Amount", det.Amount.String()},
		{"Transaction", det.TransactionID + " on " + det.TransactionDate.Format(time.DateOnly)},
		{"Respond by", due},
		{"Cardholder claim", det.CardholderClaim},
	}
}

// Cite renders a cited evidence path with its value, for example
// transaction.three_ds.status = "authenticated".
func Cite(ev domain.EvidenceSet, path string) string {
	v, ok := ev.Lookup(path)
	if !ok {
		return path + " (not available)"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return path + " = " + s
}

func (p Package) evidenceValue(path string) string { return Cite(p.Evidence, path) }

// models and prompts used, from the audit trail.
func (p Package) provenance() (models, prompts []string, in, out int) {
	ms, ps := map[string]bool{}, map[string]bool{}
	for _, ev := range p.Audit {
		if ev.Kind != domain.AuditLLMCall {
			continue
		}
		if ev.Model != "" {
			ms[ev.Model] = true
		}
		if ev.PromptVersion != "" {
			ps[ev.PromptVersion] = true
		}
		in += ev.InputTokens
		out += ev.OutputTokens
	}
	return keys(ms), keys(ps), in, out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func networkName(n domain.Network) string {
	switch n {
	case domain.NetworkVisa:
		return "Visa"
	case domain.NetworkMastercard:
		return "Mastercard"
	}
	return string(n)
}

func categoryName(c domain.Category) string {
	switch c {
	case domain.CategoryFraud:
		return "fraud"
	case domain.CategoryNotReceived:
		return "not received"
	case domain.CategoryNotAsDescribed:
		return "not as described"
	case domain.CategoryDuplicate:
		return "duplicate"
	case domain.CategoryCreditNotProcessed:
		return "credit not processed"
	}
	return "unknown"
}

func statusLabel(s domain.RequirementStatus) string {
	switch s {
	case domain.RequirementMet:
		return "Met"
	case domain.RequirementNotMet:
		return "Not met"
	case domain.RequirementMissingEvidence:
		return "Missing evidence"
	}
	return string(s)
}

func decisionLabel(r domain.Recommendation) string {
	switch r {
	case domain.RecommendFight:
		return "Fight"
	case domain.RecommendAccept:
		return "Accept"
	}
	return "-"
}
