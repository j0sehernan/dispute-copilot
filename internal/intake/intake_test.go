package intake

import (
	"errors"
	"testing"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
)

func TestParseWebhookFixtures(t *testing.T) {
	scenarios, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range scenarios {
		if s.Input.Kind != "json" {
			continue
		}
		n++
		d, err := ParseWebhook(s.Input.Payload)
		if err != nil {
			t.Errorf("%s: %v", s.ID, err)
			continue
		}
		if d.Amount.Minor <= 0 || d.RespondBy == nil {
			t.Errorf("%s: incomplete details %+v", s.ID, d)
		}
	}
	if n == 0 {
		t.Fatal("no json fixtures")
	}
}

func TestParseWebhookRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field": `{"case_id":"A","network":"visa","reason_code":"10.4","amount":{"value":"1.00","currency":"USD"},"transaction_id":"t","transaction_date":"2026-01-01","pan":"4111111111111111"}`,
		"bad network":   `{"case_id":"A","network":"amex","reason_code":"10.4","amount":{"value":"1.00","currency":"USD"},"transaction_id":"t","transaction_date":"2026-01-01"}`,
		"float amount":  `{"case_id":"A","network":"visa","reason_code":"10.4","amount":{"value":"1.001","currency":"USD"},"transaction_id":"t","transaction_date":"2026-01-01"}`,
		"bad date":      `{"case_id":"A","network":"visa","reason_code":"10.4","amount":{"value":"1.00","currency":"USD"},"transaction_id":"t","transaction_date":"01/02/2026"}`,
		"not json":      `case A`,
	}
	for name, body := range cases {
		if _, err := ParseWebhook([]byte(body)); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestDetect(t *testing.T) {
	if Detect([]byte("%PDF-1.7 ...")) != domain.InputPDF {
		t.Error("pdf")
	}
	if Detect([]byte(` {"a":1}`)) != domain.InputJSON {
		t.Error("json")
	}
	if Detect([]byte("{ not json")) != domain.InputText {
		t.Error("broken json is text")
	}
}

func TestSignature(t *testing.T) {
	body := []byte(`{"case_id":"X"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) {
		t.Error("valid signature rejected")
	}
	if VerifySignature("other", body, sig) || VerifySignature("s3cret", []byte(`{}`), sig) {
		t.Error("invalid signature accepted")
	}
}

func TestDedupeKey(t *testing.T) {
	a := DedupeKey("", []byte("x"))
	if a != DedupeKey("", []byte("x")) || a == DedupeKey("", []byte("y")) {
		t.Error("content hash is not stable")
	}
	if DedupeKey("abc", []byte("x")) != "key:abc" {
		t.Error("idempotency key should win")
	}
}
