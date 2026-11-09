package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

// Gatherer runs the tool-calling loop that collects evidence.
type Gatherer struct {
	client   llm.Client
	records  Records
	recorder Recorder
	prompt   Prompt
	// MaxTurns caps the model round trips. Four tools rarely need more
	// than three.
	MaxTurns int
}

func NewGatherer(client llm.Client, records Records, rec Recorder) *Gatherer {
	return &Gatherer{
		client:   client,
		records:  records,
		recorder: rec,
		prompt:   loadPrompt("gather.v1"),
		MaxTurns: 6,
	}
}

func (g *Gatherer) PromptVersion() string { return g.prompt.Version }

// GatherResult is the evidence plus a note on how it was collected.
type GatherResult struct {
	Evidence domain.EvidenceSet
	// Backstop lists lookups the model skipped and we ran ourselves.
	Backstop []string
	Usage    llm.Usage
}

func (g *Gatherer) Gather(ctx context.Context, disputeID uuid.UUID, d domain.Details) (GatherResult, error) {
	sc := newScope(d.TransactionID)
	res := GatherResult{Evidence: domain.EvidenceSet{}}

	brief, _ := json.MarshalIndent(map[string]any{
		"case_id":          d.CaseID,
		"network":          d.Network,
		"reason_code":      d.ReasonCode,
		"amount":           d.Amount.String(),
		"transaction_id":   d.TransactionID,
		"transaction_date": d.TransactionDate.Format(time.DateOnly),
		"cardholder_claim": d.CardholderClaim,
	}, "", "  ")
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: g.prompt.Text},
		{Role: llm.RoleUser, Content: "Dispute:\n" + string(brief)},
	}

	for turn := 1; turn <= g.MaxTurns; turn++ {
		resp, err := g.client.Complete(ctx, llm.Request{Messages: msgs, Tools: toolDefs(), Temperature: 0})
		g.auditLLM(ctx, disputeID, resp, turn, err)
		if err != nil {
			return res, err
		}
		res.Usage.Add(resp.Usage)

		calls := resp.Message.ToolCalls
		if len(calls) == 0 {
			break
		}
		msgs = append(msgs, resp.Message)

		results, err := g.runCalls(ctx, disputeID, sc, calls, false)
		if err != nil {
			return res, err
		}
		for i, r := range results {
			msgs = append(msgs, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: calls[i].ID,
				ToolName:   calls[i].Name,
				Content:    r.content,
			})
			keep(res.Evidence, r)
		}
	}

	// The model decides the order of the lookups, but it must not decide
	// whether the evidence is complete. Anything reachable that it skipped
	// is fetched here.
	for {
		pending := sc.pending(res.Evidence)
		if len(pending) == 0 {
			break
		}
		calls := make([]llm.ToolCall, len(pending))
		for i, p := range pending {
			args, _ := json.Marshal(map[string]string{tools[p.name].arg: p.id})
			calls[i] = llm.ToolCall{ID: fmt.Sprintf("backstop_%d", i), Name: p.name, Arguments: args}
			res.Backstop = append(res.Backstop, p.name)
		}
		results, err := g.runCalls(ctx, disputeID, sc, calls, true)
		if err != nil {
			return res, err
		}
		for _, r := range results {
			keep(res.Evidence, r)
		}
	}
	return res, nil
}

// keep stores a result as evidence. Not-found is evidence too; refused calls
// and repeated lookups of a source we already have are not.
func keep(set domain.EvidenceSet, r toolResult) {
	if r.refused != "" || r.err != nil || r.source == "" {
		return
	}
	if prev, ok := set[r.source]; ok && prev.Found {
		return
	}
	set[r.source] = domain.EvidenceItem{
		Source:    r.source,
		Ref:       r.id,
		Found:     r.found,
		Data:      r.data,
		FetchedAt: time.Now().UTC(),
	}
}

// runCalls executes the calls of one model turn in parallel. Results keep
// the order of the calls, as the chat API expects.
func (g *Gatherer) runCalls(ctx context.Context, disputeID uuid.UUID, sc *scope, calls []llm.ToolCall, backstop bool) ([]toolResult, error) {
	results := make([]toolResult, len(calls))
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(4)
	for i, call := range calls {
		eg.Go(func() error {
			start := time.Now()
			r := runTool(ctx, g.records, sc, call)
			g.auditTool(ctx, disputeID, r, time.Since(start), backstop)
			results[i] = r
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	// A failing upstream is a reason to retry the job later, not to analyse
	// a dispute with holes we did not choose.
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("%s: %w", r.name, r.err)
		}
	}
	return results, nil
}

func (g *Gatherer) auditLLM(ctx context.Context, id uuid.UUID, resp *llm.Response, turn int, err error) {
	ev := domain.AuditEvent{
		DisputeID:     id,
		Step:          "gather_evidence",
		Kind:          domain.AuditLLMCall,
		Name:          "tool_loop",
		PromptVersion: g.prompt.Version,
		Model:         g.client.Model(),
		OK:            err == nil,
		Detail:        map[string]any{"turn": turn},
	}
	if resp != nil {
		ev.Model = resp.Model
		ev.InputTokens = resp.Usage.InputTokens
		ev.OutputTokens = resp.Usage.OutputTokens
		ev.Latency = resp.Latency
		ev.Detail["tool_calls"] = len(resp.Message.ToolCalls)
	}
	if err != nil {
		ev.Detail["error"] = truncate(err.Error(), 300)
	}
	g.recorder.Record(ctx, ev)
}

func (g *Gatherer) auditTool(ctx context.Context, id uuid.UUID, r toolResult, latency time.Duration, backstop bool) {
	ev := domain.AuditEvent{
		DisputeID: id,
		Step:      "gather_evidence",
		Kind:      domain.AuditToolCall,
		Name:      r.name,
		Latency:   latency,
		OK:        r.refused == "" && r.err == nil,
		// Only ids go into the audit detail, never record contents.
		Detail: map[string]any{"id": r.id, "found": r.found, "backstop": backstop},
	}
	if r.refused != "" {
		ev.Kind = domain.AuditGuardrail
		ev.Detail["refused"] = r.refused
	}
	if r.err != nil {
		ev.Detail["error"] = truncate(r.err.Error(), 300)
	}
	g.recorder.Record(ctx, ev)
}
