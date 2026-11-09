package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

// maxNoticeChars bounds what we send to the model; real notices are one or
// two pages.
const maxNoticeChars = 20_000

type extraction struct {
	CaseID          string  `json:"case_id"`
	Network         string  `json:"network"`
	ReasonCode      string  `json:"reason_code"`
	Amount          string  `json:"amount"`
	Currency        string  `json:"currency"`
	TransactionID   string  `json:"transaction_id"`
	TransactionDate string  `json:"transaction_date"`
	OpenedAt        string  `json:"opened_at"`
	RespondBy       *string `json:"respond_by"`
	CardholderClaim string  `json:"cardholder_claim"`
}

// Extractor turns a free-text chargeback notice into domain.Details.
type Extractor struct {
	call structured[extraction]
}

func NewExtractor(client llm.Client, rec Recorder) *Extractor {
	return &Extractor{call: structured[extraction]{
		client:   client,
		recorder: rec,
		prompt:   loadPrompt("extract.v1"),
		schema:   loadSchema("extraction"),
		step:     "extract",
		check: func(e extraction) error {
			_, err := e.details()
			return err
		},
	}}
}

func (x *Extractor) PromptVersion() string { return x.call.prompt.Version }

func (x *Extractor) Extract(ctx context.Context, disputeID uuid.UUID, notice string) (domain.Details, error) {
	notice = strings.TrimSpace(notice)
	if notice == "" {
		return domain.Details{}, fmt.Errorf("extract: %w: empty notice", domain.ErrInvalidInput)
	}
	if len(notice) > maxNoticeChars {
		notice = notice[:maxNoticeChars]
	}
	user := "Chargeback notice between the markers:\n<<<NOTICE\n" + notice + "\nNOTICE>>>"

	out, err := x.call.run(ctx, disputeID, user)
	if err != nil {
		return domain.Details{}, err
	}
	return out.details()
}

func (e extraction) details() (domain.Details, error) {
	amount, err := domain.ParseMoney(e.Amount, e.Currency)
	if err != nil {
		return domain.Details{}, fmt.Errorf("amount: %w", err)
	}
	txDate, err := parseDate("transaction_date", e.TransactionDate)
	if err != nil {
		return domain.Details{}, err
	}
	opened, err := parseDate("opened_at", e.OpenedAt)
	if err != nil {
		return domain.Details{}, err
	}
	d := domain.Details{
		CaseID:          strings.TrimSpace(e.CaseID),
		Network:         domain.Network(strings.ToLower(strings.TrimSpace(e.Network))),
		ReasonCode:      normalizeReasonCode(e.ReasonCode),
		Amount:          amount,
		TransactionID:   strings.TrimSpace(e.TransactionID),
		TransactionDate: txDate,
		OpenedAt:        opened,
		CardholderClaim: strings.TrimSpace(e.CardholderClaim),
	}
	if e.RespondBy != nil && *e.RespondBy != "" {
		due, err := parseDate("respond_by", *e.RespondBy)
		if err != nil {
			return domain.Details{}, err
		}
		d.RespondBy = &due
	}
	return d, d.Validate()
}

func parseDate(field, v string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: expected YYYY-MM-DD, got %q", field, v)
	}
	return t, nil
}

// normalizeReasonCode keeps only the code ("13.1 Merchandise Not Received"
// becomes "13.1").
func normalizeReasonCode(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " -("); i > 0 {
		s = s[:i]
	}
	return s
}
