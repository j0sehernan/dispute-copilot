package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/llm/llmtest"
	"github.com/j0sehernan/dispute-copilot/internal/mockpsp"
	"github.com/j0sehernan/dispute-copilot/internal/psp"
	"github.com/j0sehernan/dispute-copilot/internal/rules"
)

type memRecorder struct {
	mu     sync.Mutex
	events []domain.AuditEvent
}

func (m *memRecorder) Record(_ context.Context, ev domain.AuditEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *memRecorder) byKind(k domain.AuditKind) []domain.AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.AuditEvent
	for _, e := range m.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func newRecords(t *testing.T) *psp.Client {
	t.Helper()
	scenarios, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := mockpsp.New(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return psp.New(ts.URL, 5*time.Second)
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestGatherGuardrailsAndBackstop(t *testing.T) {
	turn := 0
	model := &llmtest.Client{Respond: func(req llm.Request) (llm.Message, error) {
		turn++
		switch turn {
		case 1:
			return llm.Message{ToolCalls: []llm.ToolCall{
				call("1", "get_transaction", `{"transaction_id":"txn_8F2K1Q"}`),
				// Not linked to this dispute: must be refused.
				call("2", "get_transaction", `{"transaction_id":"txn_Q7M2Z8"}`),
			}}, nil
		case 2:
			last := req.Messages[len(req.Messages)-1]
			if last.ToolCallID != "2" || !strings.Contains(last.Content, "not linked") {
				t.Errorf("out-of-scope lookup was not refused: %+v", last)
			}
			return llm.Message{ToolCalls: []llm.ToolCall{
				call("3", "get_order", `{"order_id":"ord_10421"}`),
				call("4", "get_customer_history", `{"customer_id":"cus_3021"}`),
				call("5", "delete_order", `{"order_id":"ord_10421"}`),
			}}, nil
		default:
			// Stops without asking for shipping.
			return llmtest.Text("Found transaction, order and history.")
		}
	}}
	rec := &memRecorder{}
	g := NewGatherer(model, newRecords(t), rec)

	res, err := g.Gather(context.Background(), uuid.New(), domain.Details{TransactionID: "txn_8F2K1Q"})
	if err != nil {
		t.Fatal(err)
	}

	for _, src := range domain.EvidenceSources {
		if !res.Evidence[src].Found {
			t.Errorf("%s not found", src)
		}
	}
	if len(res.Backstop) != 1 || res.Backstop[0] != "get_shipping" {
		t.Errorf("backstop = %v, want [get_shipping]", res.Backstop)
	}
	if res.Evidence[domain.EvidenceTransaction].Ref != "txn_8F2K1Q" {
		t.Errorf("refused lookup replaced the real transaction")
	}

	refused := map[string]bool{}
	for _, e := range rec.byKind(domain.AuditGuardrail) {
		refused[e.Detail["refused"].(string)] = true
	}
	if !refused["id_out_of_scope"] || !refused["unknown_tool"] {
		t.Errorf("guardrail events = %v", refused)
	}
	if len(rec.byKind(domain.AuditLLMCall)) != 3 {
		t.Errorf("llm calls audited = %d, want 3", len(rec.byKind(domain.AuditLLMCall)))
	}
}

func TestGatherRecordsNotFound(t *testing.T) {
	model := &llmtest.Client{Respond: func(llm.Request) (llm.Message, error) { return llmtest.Text("done") }}
	g := NewGatherer(model, newRecords(t), &memRecorder{})

	// Scenario 15: the order does not exist in the order system.
	res, err := g.Gather(context.Background(), uuid.New(), domain.Details{TransactionID: "txn_X0Q6P3"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Evidence[domain.EvidenceTransaction].Found || !res.Evidence[domain.EvidenceCustomerHistory].Found {
		t.Error("transaction and history should be found")
	}
	order, ok := res.Evidence[domain.EvidenceOrder]
	if !ok || order.Found {
		t.Errorf("order should be recorded as not found, got %+v", order)
	}
}

func TestExtractorRetriesOnce(t *testing.T) {
	good := `{"case_id":"MC-1","network":"mastercard","reason_code":"4837 - No Cardholder Authorization","amount":"312.45","currency":"USD",
		"transaction_id":"txn_1","transaction_date":"2026-09-03","opened_at":"2026-09-22","respond_by":"2026-10-22","cardholder_claim":"did not authorize"}`
	bad := strings.Replace(good, `"2026-09-03"`, `"03 Sep 2026"`, 1)

	calls := 0
	model := &llmtest.Client{Respond: func(req llm.Request) (llm.Message, error) {
		calls++
		if calls == 1 {
			return llmtest.Text(bad)
		}
		last := req.Messages[len(req.Messages)-1].Content
		if !strings.Contains(last, "transaction_date") {
			t.Errorf("retry message does not explain the error: %s", last)
		}
		return llmtest.Text("```json\n" + good + "\n```")
	}}
	rec := &memRecorder{}
	d, err := NewExtractor(model, rec).Extract(context.Background(), uuid.New(), "notice text")
	if err != nil {
		t.Fatal(err)
	}
	if d.ReasonCode != "4837" || d.Amount.Minor != 31245 || d.Network != domain.NetworkMastercard {
		t.Errorf("details = %+v", d)
	}
	ev := rec.byKind(domain.AuditLLMCall)
	if len(ev) != 2 || ev[0].OK || !ev[1].OK || ev[1].PromptVersion != "extract.v1" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestExtractorGivesUpAfterSecondInvalidAnswer(t *testing.T) {
	model := &llmtest.Client{Respond: func(llm.Request) (llm.Message, error) {
		return llmtest.Text(`{"case_id":"x"}`)
	}}
	_, err := NewExtractor(model, &memRecorder{}).Extract(context.Background(), uuid.New(), "notice")
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("err = %v, want ErrInvalidOutput", err)
	}
	if len(model.Requests()) != 2 {
		t.Errorf("requests = %d, want 2", len(model.Requests()))
	}
}

func fraudInputs(t *testing.T) (AnalysisInput, *rules.Doc) {
	t.Helper()
	lib, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := lib.ByReasonCode(domain.NetworkVisa, "10.4")
	var chunks []rules.ScoredChunk
	for _, c := range doc.Chunks {
		chunks = append(chunks, rules.ScoredChunk{Chunk: c})
	}
	ev := domain.EvidenceSet{
		domain.EvidenceTransaction:     {Found: true, Data: json.RawMessage(`{"three_ds":{"status":"authenticated"},"avs_result":"Y","cvv_result":"M"}`)},
		domain.EvidenceCustomerHistory: {Found: true, Data: json.RawMessage(`{"orders":[]}`)},
		domain.EvidenceOrder:           {Found: false},
	}
	return AnalysisInput{
		DisputeID: uuid.New(),
		Details:   domain.Details{CaseID: "VDR-1", Network: domain.NetworkVisa, ReasonCode: "10.4"},
		Evidence:  ev,
		Rules:     rules.Result{Rule: doc, Match: rules.MatchReasonCode, Chunks: chunks},
	}, doc
}

func TestCheckAnalysisRejectsInventedEvidence(t *testing.T) {
	in, doc := fraudInputs(t)
	out := analysisOutput{
		Requirements: []domain.RequirementResult{
			{ID: "authentication", Status: domain.RequirementMet, Evidence: []string{"transaction.three_ds.status"}},
			{ID: "avs_cvv_match", Status: domain.RequirementMet, Evidence: []string{"transaction.avs_result", "transaction.cvv_result"}},
			{ID: "prior_undisputed_history", Status: domain.RequirementMet, Evidence: []string{"order.device_id"}},
		},
		Recommendation: domain.RecommendFight,
		Reasons: []domain.Reason{
			{Text: "3DS", Evidence: []string{"transaction.three_ds.eci"}, Rules: []string{"visa-10.4#what-wins", "visa-99#made-up"}},
		},
	}
	err := checkAnalysis(out, doc.Requirements, in.Evidence, in.Rules.ChunkIDs())
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{`"order.device_id"`, `"transaction.three_ds.eci"`, `"visa-99#made-up"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}

	out.Requirements = out.Requirements[:2]
	err = checkAnalysis(out, doc.Requirements, in.Evidence, in.Rules.ChunkIDs())
	if err == nil || !strings.Contains(err.Error(), `"prior_undisputed_history" is missing`) {
		t.Errorf("missing requirement not reported: %v", err)
	}
}

func TestAnalyzeAppliesPolicy(t *testing.T) {
	in, _ := fraudInputs(t)
	// The model marks 3DS as met but recommends accept: the policy says fight.
	answer := `{
		"requirements": [
			{"id":"prior_undisputed_history","status":"not_met","evidence":["customer_history.orders"],"explanation":"No earlier orders."},
			{"id":"authentication","status":"met","evidence":["transaction.three_ds.status"],"explanation":"3DS authenticated."},
			{"id":"avs_cvv_match","status":"met","evidence":["transaction.avs_result","transaction.cvv_result"],"explanation":"Y and M."}
		],
		"recommendation":"accept",
		"reasons":[{"text":"Authenticated with 3DS.","evidence":["transaction.three_ds.status"],"rules":["visa-10.4#what-wins"]}],
		"missing_evidence":[]
	}`
	model := &llmtest.Client{Respond: func(llm.Request) (llm.Message, error) { return llmtest.Text(answer) }}
	rec := &memRecorder{}

	a, err := NewAnalyst(model, rec).Analyze(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Recommendation != domain.RecommendFight || !a.PolicyOverride || a.ModelRecommendation != domain.RecommendAccept {
		t.Errorf("analysis = %+v", a)
	}
	if a.Requirements[0].ID != "authentication" || a.Requirements[0].Text == "" {
		t.Errorf("requirements not in checklist order or missing text: %+v", a.Requirements[0])
	}
	if len(a.MissingEvidence) == 0 || !strings.Contains(a.MissingEvidence[0], "order") {
		t.Errorf("missing order record not reported: %v", a.MissingEvidence)
	}
	if len(rec.byKind(domain.AuditGuardrail)) != 1 {
		t.Error("policy override was not audited")
	}
}

func TestCheckLetter(t *testing.T) {
	det := domain.Details{CaseID: "VDR-26-0004817"}
	ok := letterOutput{Subject: "Response", Body: "Case VDR-26-0004817. Card ending 4417. Tracking 1Z84X0E70394471182."}
	if err := checkLetter(ok, det); err != nil {
		t.Errorf("valid letter rejected: %v", err)
	}

	bad := letterOutput{Subject: "Response", Body: "Card 4111 1111 1111 1111, contact dana@example.com from 203.0.113.24"}
	err := checkLetter(bad, det)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"case number", "card number", "email", "IP address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
