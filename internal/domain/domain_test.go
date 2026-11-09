package domain

import (
	"encoding/json"
	"testing"
)

func TestParseMoney(t *testing.T) {
	tests := []struct {
		value, currency string
		want            int64
		wantErr         bool
	}{
		{"129.90", "USD", 12990, false},
		{"1,299.9", "usd", 129990, false},
		{"45", "EUR", 4500, false},
		{"12000", "JPY", 12000, false},
		{"12.5", "JPY", 0, true},
		{"10.123", "USD", 0, true},
		{"-3.00", "USD", 0, true},
		{"abc", "USD", 0, true},
		{"10.00", "US", 0, true},
		{"", "USD", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseMoney(tt.value, tt.currency)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseMoney(%q, %q) error = %v, wantErr %v", tt.value, tt.currency, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got.Minor != tt.want {
			t.Errorf("ParseMoney(%q, %q) = %d, want %d", tt.value, tt.currency, got.Minor, tt.want)
		}
	}
}

func TestMoneyDecimal(t *testing.T) {
	cases := map[Money]string{
		{Minor: 12990, Currency: "USD"}: "129.90",
		{Minor: 5, Currency: "USD"}:     "0.05",
		{Minor: 12000, Currency: "JPY"}: "12000",
		{Minor: 1500, Currency: "KWD"}:  "1.500",
	}
	for m, want := range cases {
		if got := m.Decimal(); got != want {
			t.Errorf("%+v.Decimal() = %q, want %q", m, got, want)
		}
	}
}

func TestEvidenceLookup(t *testing.T) {
	set := EvidenceSet{
		EvidenceShipping: {
			Source: EvidenceShipping,
			Found:  true,
			Data:   json.RawMessage(`{"status":"delivered","proof":{"type":"signature","signed_by":null},"events":[{"code":"DL"}]}`),
		},
		EvidenceOrder: {Source: EvidenceOrder, Found: false},
	}

	if v, ok := set.Lookup("shipping.proof.type"); !ok || v != "signature" {
		t.Errorf("shipping.proof.type = %v, %v", v, ok)
	}
	if _, ok := set.Lookup("shipping.events.0.code"); !ok {
		t.Error("expected array index to resolve")
	}
	if _, ok := set.Lookup("shipping.events[0].code"); !ok {
		t.Error("expected bracket index to resolve")
	}
	for _, path := range []string{
		"shipping.proof.signed_by", // null is not evidence
		"shipping.proof.photo_url",
		"shipping.events.3.code",
		"order.id", // not found
		"transaction.id",
	} {
		if v, ok := set.Lookup(path); ok {
			t.Errorf("Lookup(%q) = %v, want missing", path, v)
		}
	}

	missing := set.Missing()
	if len(missing) != 3 {
		t.Errorf("Missing() = %v, want transaction, order and customer_history", missing)
	}
}

func TestPolicyDecide(t *testing.T) {
	p := Policy{FightWhen: [][]string{{"authentication"}, {"avs_cvv", "prior_history"}}}

	res := func(statuses map[string]RequirementStatus) []RequirementResult {
		var out []RequirementResult
		for id, s := range statuses {
			out = append(out, RequirementResult{ID: id, Status: s})
		}
		return out
	}

	tests := []struct {
		name string
		in   map[string]RequirementStatus
		want Recommendation
	}{
		{"3ds alone", map[string]RequirementStatus{"authentication": RequirementMet}, RecommendFight},
		{"avs and history", map[string]RequirementStatus{"avs_cvv": RequirementMet, "prior_history": RequirementMet}, RecommendFight},
		{"avs only", map[string]RequirementStatus{"avs_cvv": RequirementMet, "prior_history": RequirementNotMet}, RecommendAccept},
		{"missing evidence", map[string]RequirementStatus{"authentication": RequirementMissingEvidence}, RecommendAccept},
		{"nothing", nil, RecommendAccept},
	}
	for _, tt := range tests {
		if got := p.Decide(res(tt.in)); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}
