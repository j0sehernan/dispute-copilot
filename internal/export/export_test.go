package export

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

func samplePackage(status domain.Status) Package {
	approved := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	return Package{
		Dispute: &domain.Dispute{
			ID:       uuid.New(),
			Status:   status,
			Category: domain.CategoryNotReceived,
			Details: &domain.Details{
				CaseID: "VDR-26-0004990", Network: domain.NetworkVisa, ReasonCode: "13.1",
				Amount:          domain.Money{Minor: 18430, Currency: "USD"},
				TransactionID:   "txn_M5B7J2",
				TransactionDate: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
				CardholderClaim: "Never got the package | Tomás",
			},
			Analysis: &domain.Analysis{
				RuleID:         "visa-13.1",
				Recommendation: domain.RecommendFight,
				Requirements: []domain.RequirementResult{{
					ID: "delivered", Text: "Delivered", Status: domain.RequirementMet,
					Evidence: []string{"shipping.status"}, Explanation: "Status is delivered.",
				}},
				Reasons: []domain.Reason{{Text: "Signed delivery.", Evidence: []string{"shipping.proof.signed_by"}}},
			},
			Decision:   domain.RecommendFight,
			ReviewedBy: "Ana",
			ApprovedAt: &approved,
			Letter:     domain.Letter{Subject: "Case VDR-26-0004990", Body: "Dear dispute team,\n\nThe parcel was delivered."},
		},
		Evidence: domain.EvidenceSet{
			domain.EvidenceShipping: {Source: domain.EvidenceShipping, Ref: "ord_10489", Found: true,
				Data: json.RawMessage(`{"status":"delivered","proof":{"type":"signature","signed_by":"P NATARAJAN"}}`)},
			domain.EvidenceOrder: {Source: domain.EvidenceOrder, Ref: "ord_10489", Found: false},
		},
	}
}

func TestMarkdown(t *testing.T) {
	md := string(samplePackage(domain.StatusApproved).Markdown())
	for _, want := range []string{
		"# Dispute package VDR-26-0004990",
		"Approved by Ana on 2026-10-02",
		`shipping.proof.signed_by = "P NATARAJAN"`,
		"Never got the package \\| Tomás",
		"Not found (looked up `ord_10489`)",
		"The parcel was delivered.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}

	draft := string(samplePackage(domain.StatusReadyForReview).Markdown())
	if !strings.Contains(draft, "DRAFT") {
		t.Error("unapproved package must be marked as draft")
	}
}

func TestPDF(t *testing.T) {
	out, err := samplePackage(domain.StatusApproved).PDF()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) || len(out) < 1000 {
		t.Errorf("not a pdf (%d bytes)", len(out))
	}
}

func TestFileName(t *testing.T) {
	p := samplePackage(domain.StatusApproved)
	p.Dispute.Details.CaseID = "MC/CB 2026"
	if got := p.FileName("pdf"); got != "dispute-MC-CB-2026.pdf" {
		t.Errorf("FileName = %q", got)
	}
}
