// Package fixtures holds the synthetic demo data. Each scenario is one
// dispute plus the records the mock PSP returns for it and the labels the
// eval expects. Everything here is invented: names, addresses (documentation
// IP ranges, example.com emails), card suffixes and case numbers.
package fixtures

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
)

//go:embed scenarios/*.json
var files embed.FS

type Scenario struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Input       Input    `json:"input"`
	Expected    Expected `json:"expected"`
	Data        Data     `json:"data"`
}

// Input is the dispute as it arrives: a structured webhook payload or the
// text of an acquirer notice.
type Input struct {
	Kind    string          `json:"kind"` // json | text
	Payload json.RawMessage `json:"payload,omitempty"`
	Text    string          `json:"text,omitempty"`
}

type Expected struct {
	Category       string   `json:"category"`
	Recommendation string   `json:"recommendation"`
	Evidence       []string `json:"evidence"`
}

type Data struct {
	Transaction     json.RawMessage `json:"transaction,omitempty"`
	Order           json.RawMessage `json:"order,omitempty"`
	Shipping        json.RawMessage `json:"shipping,omitempty"`
	CustomerHistory json.RawMessage `json:"customer_history,omitempty"`
}

// Load returns all scenarios sorted by id.
func Load() ([]Scenario, error) {
	entries, err := files.ReadDir("scenarios")
	if err != nil {
		return nil, err
	}
	out := make([]Scenario, 0, len(entries))
	for _, e := range entries {
		b, err := files.ReadFile(path.Join("scenarios", e.Name()))
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
