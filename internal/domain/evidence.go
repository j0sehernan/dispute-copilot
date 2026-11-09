package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type EvidenceSource string

const (
	EvidenceTransaction     EvidenceSource = "transaction"
	EvidenceOrder           EvidenceSource = "order"
	EvidenceShipping        EvidenceSource = "shipping"
	EvidenceCustomerHistory EvidenceSource = "customer_history"
)

// EvidenceSources is the fixed order used when rendering evidence.
var EvidenceSources = []EvidenceSource{
	EvidenceTransaction,
	EvidenceOrder,
	EvidenceShipping,
	EvidenceCustomerHistory,
}

// EvidenceItem is the result of one lookup against the merchant/PSP systems.
// Found is false when the agent asked for the record and it does not exist,
// which is itself useful information for the reviewer.
type EvidenceItem struct {
	Source    EvidenceSource
	Ref       string
	Found     bool
	Data      json.RawMessage
	FetchedAt time.Time
}

type EvidenceSet map[EvidenceSource]EvidenceItem

// Lookup resolves a citation like "shipping.proof.type" or
// "customer_history.orders.0.ip" against the gathered evidence. It returns
// false when the path does not exist or points to null, so the validator can
// reject answers that cite facts we never saw.
func (s EvidenceSet) Lookup(path string) (any, bool) {
	// Models often write "orders[0].ip"; treat it as "orders.0.ip".
	path = strings.NewReplacer("[", ".", "]", "").Replace(strings.TrimSpace(path))
	parts := strings.Split(path, ".")
	item, ok := s[EvidenceSource(parts[0])]
	if !ok || !item.Found {
		return nil, false
	}

	var cur any
	if err := json.Unmarshal(item.Data, &cur); err != nil {
		return nil, false
	}
	for _, p := range parts[1:] {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[p]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, cur != nil
}

// Missing lists sources that were looked up but not found, plus sources that
// were never looked up.
func (s EvidenceSet) Missing() []EvidenceSource {
	var out []EvidenceSource
	for _, src := range EvidenceSources {
		if item, ok := s[src]; !ok || !item.Found {
			out = append(out, src)
		}
	}
	return out
}
