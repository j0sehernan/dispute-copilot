// Package domain holds the core types of the dispute workflow. It has no
// dependencies on storage, transport or LLM providers.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a dispute. The happy path is
// received → extracting → gathering_evidence → analyzing → ready_for_review → approved.
type Status string

const (
	StatusReceived          Status = "received"
	StatusExtracting        Status = "extracting"
	StatusGatheringEvidence Status = "gathering_evidence"
	StatusAnalyzing         Status = "analyzing"
	StatusReadyForReview    Status = "ready_for_review"
	StatusApproved          Status = "approved"
	StatusFailed            Status = "failed"
)

// Processing reports whether the agent is still working on the dispute.
func (s Status) Processing() bool {
	switch s {
	case StatusReceived, StatusExtracting, StatusGatheringEvidence, StatusAnalyzing:
		return true
	}
	return false
}

type Network string

const (
	NetworkVisa       Network = "visa"
	NetworkMastercard Network = "mastercard"
)

func (n Network) Valid() bool { return n == NetworkVisa || n == NetworkMastercard }

// Category groups reason codes that need the same kind of evidence.
type Category string

const (
	CategoryFraud              Category = "fraud"
	CategoryNotReceived        Category = "not_received"
	CategoryNotAsDescribed     Category = "not_as_described"
	CategoryDuplicate          Category = "duplicate"
	CategoryCreditNotProcessed Category = "credit_not_processed"
)

type Source string

const (
	SourceWebhook Source = "webhook"
	SourceUpload  Source = "upload"
)

// InputKind is the format the dispute arrived in. Structured JSON skips the
// LLM extraction step; text and PDF notices go through it.
type InputKind string

const (
	InputJSON InputKind = "json"
	InputText InputKind = "text"
	InputPDF  InputKind = "pdf"
)

// Details are the fields we need from the chargeback notice.
type Details struct {
	CaseID          string
	Network         Network
	ReasonCode      string
	Amount          Money
	TransactionID   string
	TransactionDate time.Time
	OpenedAt        time.Time
	RespondBy       *time.Time
	CardholderClaim string
}

func (d Details) Validate() error {
	var errs []error
	if d.CaseID == "" {
		errs = append(errs, errors.New("case id is required"))
	}
	if !d.Network.Valid() {
		errs = append(errs, errors.New("network must be visa or mastercard"))
	}
	if d.ReasonCode == "" {
		errs = append(errs, errors.New("reason code is required"))
	}
	if d.Amount.Minor <= 0 {
		errs = append(errs, errors.New("amount must be positive"))
	}
	if d.TransactionID == "" {
		errs = append(errs, errors.New("transaction id is required"))
	}
	if d.TransactionDate.IsZero() {
		errs = append(errs, errors.New("transaction date is required"))
	}
	if !d.OpenedAt.IsZero() && d.OpenedAt.Before(d.TransactionDate) {
		errs = append(errs, errors.New("dispute opened before the transaction date"))
	}
	return errors.Join(errs...)
}

type Dispute struct {
	ID        uuid.UUID
	Source    Source
	InputKind InputKind
	RawInput  string
	DedupeKey string
	Status    Status

	Details  *Details
	Category Category
	RuleID   string

	Analysis *Analysis
	Letter   Letter

	// Decision is what the reviewer chose. It starts empty and can differ
	// from the agent's recommendation.
	Decision   Recommendation
	ReviewedBy string

	FailureReason string

	CreatedAt  time.Time
	UpdatedAt  time.Time
	ReadyAt    *time.Time
	ApprovedAt *time.Time
}

// Review is what a human submits from the review screen.
type Review struct {
	Reviewer string
	Decision Recommendation
	Letter   Letter
}

type Letter struct {
	Subject string
	Body    string
	Edited  bool
}

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidState  = errors.New("action not allowed in the current status")
	ErrInvalidInput  = errors.New("invalid input")
	ErrAlreadyExists = errors.New("already exists")
)
