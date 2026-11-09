package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	in := "customer dana.w@example.com from 203.0.113.24 paid with 4111 1111 1111 1111, tracking 9400111899223817465512"
	got := String(in)

	for _, leak := range []string{"dana.w@example.com", "203.0.113.24", "4111 1111 1111 1111"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q still in %q", leak, got)
		}
	}
	if !strings.Contains(got, "9400111899223817465512") {
		t.Errorf("tracking number should be kept: %q", got)
	}
}

func TestLooksLikePAN(t *testing.T) {
	cases := map[string]bool{
		"4111111111111111":    true,
		"5555-5555-5555-4444": true,
		"4111111111111112":    false, // fails Luhn
		"123456789012":        false, // too short
		"7720193844100000000": false,
	}
	for in, want := range cases {
		if got := LooksLikePAN(in); got != want {
			t.Errorf("LooksLikePAN(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLoggerDropsPII(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}))
	log.Info("lookup",
		"email", "x@example.com",
		"ip", "192.0.2.1",
		"note", "contact x@example.com",
		"err", errors.New("order for y@example.com not found"),
		"order_id", "ord_10421",
	)

	out := buf.String()
	if strings.Contains(out, "@example.com") || strings.Contains(out, "192.0.2.1") {
		t.Errorf("PII in log line: %s", out)
	}
	if !strings.Contains(out, "ord_10421") {
		t.Errorf("non-sensitive field was dropped: %s", out)
	}
}
