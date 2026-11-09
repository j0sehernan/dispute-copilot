package pipeline_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/j0sehernan/dispute-copilot/internal/agent"
	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/export"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/llm/llmtest"
	"github.com/j0sehernan/dispute-copilot/internal/mockpsp"
	"github.com/j0sehernan/dispute-copilot/internal/pipeline"
	"github.com/j0sehernan/dispute-copilot/internal/psp"
	"github.com/j0sehernan/dispute-copilot/internal/queue"
	"github.com/j0sehernan/dispute-copilot/internal/rules"
	"github.com/j0sehernan/dispute-copilot/internal/service"
	"github.com/j0sehernan/dispute-copilot/internal/store"
)

// testDB creates a throwaway database so tests never touch real data.
func testDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := "copilot_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

var (
	checklistRe = regexp.MustCompile(`(?m)^- ([a-z_]+): `)
	chunkRe     = regexp.MustCompile(`\[([a-z0-9.\-]+#[a-z0-9\-]+)\]`)
	txnRe       = regexp.MustCompile(`"transaction_id": "([^"]+)"`)
)

// fakeAgent behaves like a careful model: it extracts the notice, walks the
// tools in order and answers every requirement citing real fields.
func fakeAgent(t *testing.T) *llmtest.Client {
	return &llmtest.Client{Respond: func(req llm.Request) (llm.Message, error) {
		user := req.Messages[1].Content
		switch {
		case req.Schema != nil && req.Schema.Name == "extraction":
			if strings.Contains(user, "BROKEN") {
				return llmtest.Text(`{"case_id": ""}`)
			}
			return llmtest.Text(`{"case_id":"MC-CB-2026-118204","network":"mastercard","reason_code":"4837",
				"amount":"312.45","currency":"USD","transaction_id":"txn_Q7M2Z8","transaction_date":"2026-09-03",
				"opened_at":"2026-09-22","respond_by":"2026-10-22","cardholder_claim":"Did not authorize."}`)

		case len(req.Tools) > 0:
			return nextToolCall(t, req)

		case req.Schema != nil && req.Schema.Name == "analysis":
			var reqs []map[string]any
			for _, m := range checklistRe.FindAllStringSubmatch(user, -1) {
				reqs = append(reqs, map[string]any{
					"id": m[1], "status": "met", "evidence": []string{"transaction.avs_result"},
					"explanation": "AVS result Y.",
				})
			}
			out, _ := json.Marshal(map[string]any{
				"requirements":   reqs,
				"recommendation": "fight",
				"reasons": []map[string]any{{
					"text": "The checks passed.", "evidence": []string{"transaction.cvv_result"},
					"rules": []string{chunkRe.FindStringSubmatch(user)[1]},
				}},
				"missing_evidence": []string{},
			})
			return llmtest.Text(string(out))

		case req.Schema != nil && req.Schema.Name == "letter":
			caseID := regexp.MustCompile(`Case number: (\S+)`).FindStringSubmatch(user)[1]
			body := "Dear dispute team,\n\nWe respond to case " + caseID + ". " + strings.Repeat("The evidence shows the cardholder made this purchase. ", 5)
			out, _ := json.Marshal(map[string]string{"subject": "Representment " + caseID, "body": body})
			return llmtest.Text(string(out))
		}
		t.Errorf("unexpected request: %+v", req.Schema)
		return llmtest.Text("{}")
	}}
}

func nextToolCall(t *testing.T, req llm.Request) (llm.Message, error) {
	seen := map[string]json.RawMessage{}
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			seen[m.ToolName] = json.RawMessage(m.Content)
		}
	}
	call := func(name, arg, id string) (llm.Message, error) {
		args, _ := json.Marshal(map[string]string{arg: id})
		return llm.Message{ToolCalls: []llm.ToolCall{{ID: name, Name: name, Arguments: args}}}, nil
	}
	if _, ok := seen["get_transaction"]; !ok {
		return call("get_transaction", "transaction_id", txnRe.FindStringSubmatch(req.Messages[1].Content)[1])
	}
	var txn struct {
		OrderID    string `json:"order_id"`
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(seen["get_transaction"], &txn); err != nil {
		t.Fatalf("transaction result: %v", err)
	}
	switch {
	case seen["get_order"] == nil:
		return call("get_order", "order_id", txn.OrderID)
	case seen["get_shipping"] == nil:
		return call("get_shipping", "order_id", txn.OrderID)
	case seen["get_customer_history"] == nil:
		return call("get_customer_history", "customer_id", txn.CustomerID)
	}
	return llmtest.Text("All four records found.")
}

func TestPipelineEndToEnd(t *testing.T) {
	dbURL := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(ctx, dbURL, log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate should be a no-op: %v", err)
	}

	scenarios, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	mock, err := mockpsp.New(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	pspSrv := httptest.NewServer(mock.Handler())
	defer pspSrv.Close()

	lib, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	retriever := rules.NewRetriever(lib, st, llm.HashEmbedder{})
	if _, err := retriever.Index(ctx); err != nil {
		t.Fatal(err)
	}

	model := fakeAgent(t)
	p := &pipeline.Pipeline{
		Store:     st,
		Recorder:  st,
		Rules:     lib,
		Extractor: agent.NewExtractor(model, st),
		Gatherer:  agent.NewGatherer(model, psp.New(pspSrv.URL, 5*time.Second), st),
		Retriever: retriever,
		Analyst:   agent.NewAnalyst(model, st),
		Drafter:   agent.NewDrafter(model, st),
		Log:       log,
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		queue.NewWorker(st, p.Handlers(), log, queue.Options{
			Concurrency: 3, Poll: 50 * time.Millisecond, Lease: time.Minute,
			Wake: st.ListenJobs(workerCtx), OnFailure: p.OnFailure,
		}).Run(workerCtx)
		close(done)
	}()
	defer func() { stopWorker(); <-done }()

	svc := service.NewDisputes(st)
	byID := map[string]fixtures.Scenario{}
	for _, s := range scenarios {
		byID[s.ID] = s
	}

	text, err := svc.Submit(ctx, service.Submission{Source: domain.SourceUpload, Content: []byte(byID["02-fraud-repeat-customer-same-device"].Input.Text)})
	if err != nil {
		t.Fatal(err)
	}
	webhook, err := svc.Submit(ctx, service.Submission{Source: domain.SourceWebhook, Content: byID["01-fraud-3ds-authenticated"].Input.Payload})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := svc.Submit(ctx, service.Submission{Source: domain.SourceUpload, Content: []byte("BROKEN notice that the model cannot read, padded to pass the length check.")})
	if err != nil {
		t.Fatal(err)
	}

	again, err := svc.Submit(ctx, service.Submission{Source: domain.SourceWebhook, Content: byID["01-fraud-3ds-authenticated"].Input.Payload})
	if err != nil || again.Created || again.ID != webhook.ID {
		t.Fatalf("duplicate webhook created a new dispute: %+v %v", again, err)
	}

	wait := func(id uuid.UUID, want domain.Status) *service.View {
		t.Helper()
		for {
			v, err := svc.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if v.Dispute.Status == want {
				return v
			}
			if !v.Dispute.Status.Processing() {
				t.Fatalf("dispute %s ended as %s (%s), want %s", id, v.Dispute.Status, v.Dispute.FailureReason, want)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("timeout waiting for %s, status %s", id, v.Dispute.Status)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	v := wait(text.ID, domain.StatusReadyForReview)
	d := v.Dispute
	if d.Details.CaseID != "MC-CB-2026-118204" || d.Category != domain.CategoryFraud || d.RuleID != "mc-4837" {
		t.Errorf("extraction: %+v category=%s rule=%s", d.Details, d.Category, d.RuleID)
	}
	if len(v.Evidence) != 4 {
		t.Errorf("evidence sources = %d, want 4", len(v.Evidence))
	}
	if d.Analysis.Recommendation != domain.RecommendFight || d.Decision != domain.RecommendFight {
		t.Errorf("recommendation %s, decision %s", d.Analysis.Recommendation, d.Decision)
	}
	if !strings.Contains(d.Letter.Body, "MC-CB-2026-118204") {
		t.Errorf("letter: %q", d.Letter.Body)
	}
	assertAudit(t, v.Audit)

	wv := wait(webhook.ID, domain.StatusReadyForReview)
	if wv.Dispute.InputKind != domain.InputJSON || wv.Dispute.RuleID != "visa-10.4" {
		t.Errorf("webhook dispute: kind %s rule %s", wv.Dispute.InputKind, wv.Dispute.RuleID)
	}
	for _, ev := range wv.Audit {
		if ev.Step == "extract" && ev.Kind == domain.AuditLLMCall {
			t.Error("structured webhook should not call the model for extraction")
		}
	}

	bv := wait(broken.ID, domain.StatusFailed)
	if !strings.Contains(bv.Dispute.FailureReason, "failed validation") {
		t.Errorf("failure reason = %q", bv.Dispute.FailureReason)
	}
	extractCalls := 0
	for _, ev := range bv.Audit {
		if ev.Step == "extract" && ev.Kind == domain.AuditLLMCall {
			extractCalls++
		}
	}
	if extractCalls != 2 {
		t.Errorf("extraction attempts = %d, want 2 (one retry)", extractCalls)
	}

	// Human review.
	err = svc.Approve(ctx, text.ID, service.ReviewInput{
		Reviewer: "Ana Ruiz", Decision: "fight", Subject: d.Letter.Subject, Body: d.Letter.Body + "\n\nRegards.",
	})
	if err != nil {
		t.Fatal(err)
	}
	av := wait(text.ID, domain.StatusApproved)
	if !av.Dispute.Letter.Edited || av.Dispute.ReviewedBy != "Ana Ruiz" || av.Dispute.ApprovedAt == nil {
		t.Errorf("approved dispute: %+v", av.Dispute)
	}
	if err := svc.SaveDraft(ctx, text.ID, service.ReviewInput{Reviewer: "x", Decision: "accept"}); err == nil {
		t.Error("editing an approved dispute must fail")
	}

	md := string(export.Package{Dispute: av.Dispute, Evidence: av.Evidence, Audit: av.Audit}.Markdown())
	if !strings.Contains(md, "Approved by Ana Ruiz") || !strings.Contains(md, "transaction.avs_result = \"Y\"") {
		t.Errorf("package markdown:\n%s", md)
	}
}

func assertAudit(t *testing.T, events []domain.AuditEvent) {
	t.Helper()
	var statuses []string
	prompts := map[string]bool{}
	tools := map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case domain.AuditStatus:
			statuses = append(statuses, ev.Name)
		case domain.AuditLLMCall:
			prompts[ev.PromptVersion] = true
			if ev.Model == "" || ev.InputTokens == 0 {
				t.Errorf("llm event without model or tokens: %+v", ev)
			}
		case domain.AuditToolCall:
			tools[ev.Name] = true
		}
		raw, _ := json.Marshal(ev.Detail)
		if strings.Contains(string(raw), "@example.com") {
			t.Errorf("personal data in audit detail: %s", raw)
		}
	}
	want := "received,extracting,gathering_evidence,analyzing,ready_for_review"
	if got := strings.Join(statuses, ","); got != want {
		t.Errorf("status trail = %s", got)
	}
	for _, p := range []string{"extract.v1", "gather.v1", "analyze.v1", "letter.v1"} {
		if !prompts[p] {
			t.Errorf("no llm call recorded with prompt %s (have %v)", p, prompts)
		}
	}
	if len(tools) != 4 {
		t.Errorf("tools used = %v", tools)
	}
}
