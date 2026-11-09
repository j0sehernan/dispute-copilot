// Package intake turns what arrives (a webhook payload, an uploaded file)
// into input the pipeline can work with.
package intake

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

const MaxUploadBytes = 5 << 20

// Webhook is the structured dispute payload our (fictional) acquirer sends.
type Webhook struct {
	CaseID     string `json:"case_id"`
	Network    string `json:"network"`
	ReasonCode string `json:"reason_code"`
	Amount     struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	TransactionID   string `json:"transaction_id"`
	TransactionDate string `json:"transaction_date"`
	OpenedAt        string `json:"opened_at"`
	RespondBy       string `json:"respond_by"`
	CardholderClaim string `json:"cardholder_claim"`
}

// ParseWebhook validates a structured payload. Structured input does not
// need the LLM: the fields are already there.
func ParseWebhook(raw []byte) (domain.Details, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w Webhook
	if err := dec.Decode(&w); err != nil {
		return domain.Details{}, fmt.Errorf("%w: %w", domain.ErrInvalidInput, err)
	}

	amount, err := domain.ParseMoney(w.Amount.Value, w.Amount.Currency)
	if err != nil {
		return domain.Details{}, fmt.Errorf("%w: amount: %w", domain.ErrInvalidInput, err)
	}
	d := domain.Details{
		CaseID:          strings.TrimSpace(w.CaseID),
		Network:         domain.Network(strings.ToLower(strings.TrimSpace(w.Network))),
		ReasonCode:      strings.TrimSpace(w.ReasonCode),
		Amount:          amount,
		TransactionID:   strings.TrimSpace(w.TransactionID),
		CardholderClaim: strings.TrimSpace(w.CardholderClaim),
	}
	var errs []error
	if d.TransactionDate, err = date("transaction_date", w.TransactionDate, true); err != nil {
		errs = append(errs, err)
	}
	if d.OpenedAt, err = date("opened_at", w.OpenedAt, false); err != nil {
		errs = append(errs, err)
	}
	if w.RespondBy != "" {
		due, err := date("respond_by", w.RespondBy, true)
		if err != nil {
			errs = append(errs, err)
		}
		d.RespondBy = &due
	}
	if err := errors.Join(append(errs, d.Validate())...); err != nil {
		return domain.Details{}, fmt.Errorf("%w: %w", domain.ErrInvalidInput, err)
	}
	return d, nil
}

func date(field, v string, required bool) (time.Time, error) {
	if v == "" && !required {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return t, nil
}

// Detect classifies an upload by content, not by file name.
func Detect(data []byte) domain.InputKind {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return domain.InputPDF
	case len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed):
		return domain.InputJSON
	default:
		return domain.InputText
	}
}

// PDFText extracts the plain text of a PDF notice.
func PDFText(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("%w: unreadable pdf: %w", domain.ErrInvalidInput, err)
	}
	plain, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("%w: pdf text: %w", domain.ErrInvalidInput, err)
	}
	b, err := io.ReadAll(io.LimitReader(plain, MaxUploadBytes))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", fmt.Errorf("%w: the pdf has no text layer (scanned image?)", domain.ErrInvalidInput)
	}
	return text, nil
}

// CleanText checks a text notice.
func CleanText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", domain.ErrInvalidInput)
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if len(text) < 40 {
		return "", fmt.Errorf("%w: notice is too short", domain.ErrInvalidInput)
	}
	return text, nil
}

// DedupeKey identifies a delivery. A client-supplied Idempotency-Key wins;
// otherwise identical content counts as the same dispute.
func DedupeKey(idempotencyKey string, content []byte) string {
	if k := strings.TrimSpace(idempotencyKey); k != "" {
		return "key:" + k
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifySignature checks an HMAC-SHA256 hex signature of the body, the
// usual scheme for payment webhooks.
func VerifySignature(secret string, body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	return hmac.Equal([]byte(want), []byte(got))
}

// Sign is the counterpart of VerifySignature, used by the seed tool.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
