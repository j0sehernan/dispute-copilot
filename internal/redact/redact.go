// Package redact removes personal data from strings and log attributes.
package redact

import (
	"log/slog"
	"regexp"
	"strings"
)

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	ipv4Re  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// Card numbers: 13 to 19 digits, optionally split by spaces or dashes.
	panRe = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)
)

// sensitiveKeys are log attribute keys whose value is always hidden.
var sensitiveKeys = map[string]bool{
	"email": true, "name": true, "customer_name": true, "address": true,
	"billing_address": true, "shipping_address": true, "ip": true, "phone": true,
	"card_number": true, "pan": true, "api_key": true, "authorization": true,
	"raw_input": true, "notice": true,
}

const mask = "[redacted]"

// String masks emails, IPv4 addresses and anything that passes the Luhn
// check as a card number.
func String(s string) string {
	s = emailRe.ReplaceAllString(s, "[email]")
	s = ipv4Re.ReplaceAllString(s, "[ip]")
	return panRe.ReplaceAllStringFunc(s, func(m string) string {
		if LooksLikePAN(m) {
			return "[card]"
		}
		return m
	})
}

// LooksLikePAN reports whether s is 13-19 digits (ignoring spaces and
// dashes) and passes the Luhn checksum.
func LooksLikePAN(s string) bool {
	digits := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ContainsPAN reports whether the text holds something that looks like a
// full card number.
func ContainsPAN(s string) bool {
	for _, m := range panRe.FindAllString(s, -1) {
		if LooksLikePAN(m) {
			return true
		}
	}
	return false
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr that hides sensitive keys
// and scrubs string values.
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, mask)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, String(err.Error()))
		}
	}
	return a
}
