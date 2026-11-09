package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
	"github.com/j0sehernan/dispute-copilot/internal/service"
)

type fakeDisputes struct {
	view      *service.View
	submitted []service.Submission
	approved  *service.ReviewInput
}

func (f *fakeDisputes) Submit(_ context.Context, sub service.Submission) (service.SubmitResult, error) {
	f.submitted = append(f.submitted, sub)
	if strings.Contains(string(sub.Content), "invalid") {
		return service.SubmitResult{}, fmt.Errorf("%w: bad notice", domain.ErrInvalidInput)
	}
	return service.SubmitResult{ID: f.view.Dispute.ID, Created: len(f.submitted) == 1}, nil
}

func (f *fakeDisputes) Get(_ context.Context, id uuid.UUID) (*service.View, error) {
	if id != f.view.Dispute.ID {
		return nil, domain.ErrNotFound
	}
	return f.view, nil
}

func (f *fakeDisputes) List(context.Context) ([]*domain.Dispute, error) {
	return []*domain.Dispute{f.view.Dispute}, nil
}

func (f *fakeDisputes) SaveDraft(context.Context, uuid.UUID, service.ReviewInput) error { return nil }

func (f *fakeDisputes) Approve(_ context.Context, _ uuid.UUID, in service.ReviewInput) error {
	if in.Reviewer == "" {
		return fmt.Errorf("%w: reviewer name is required", domain.ErrInvalidInput)
	}
	f.approved = &in
	return nil
}

func readyView() *service.View {
	now := time.Now()
	due := now.AddDate(0, 0, 20)
	return &service.View{
		Dispute: &domain.Dispute{
			ID:        uuid.New(),
			Status:    domain.StatusReadyForReview,
			Category:  domain.CategoryFraud,
			CreatedAt: now.Add(-time.Minute),
			ReadyAt:   &now,
			Details: &domain.Details{
				CaseID: "VDR-26-0004817", Network: domain.NetworkVisa, ReasonCode: "10.4",
				Amount: domain.Money{Minor: 24900, Currency: "USD"}, TransactionID: "txn_8F2K1Q",
				TransactionDate: now.AddDate(0, -1, 0), RespondBy: &due, CardholderClaim: "Not me <script>",
			},
			Analysis: &domain.Analysis{
				RuleID: "visa-10.4", Recommendation: domain.RecommendFight, ModelRecommendation: domain.RecommendAccept,
				PolicyOverride: true,
				Requirements: []domain.RequirementResult{{
					ID: "authentication", Text: "3DS authenticated", Status: domain.RequirementMet,
					Evidence: []string{"transaction.three_ds.status"}, Explanation: "Authenticated.",
				}},
				Reasons:         []domain.Reason{{Text: "3DS shifts liability.", Evidence: []string{"transaction.three_ds.status"}, Rules: []string{"visa-10.4#what-wins"}}},
				MissingEvidence: []string{"order record not available"},
			},
			Decision: domain.RecommendFight,
			Letter:   domain.Letter{Subject: "Case VDR-26-0004817", Body: "Dear dispute team"},
		},
		Evidence: domain.EvidenceSet{
			domain.EvidenceTransaction: {Source: domain.EvidenceTransaction, Ref: "txn_8F2K1Q", Found: true,
				Data: json.RawMessage(`{"three_ds":{"status":"authenticated"}}`)},
			domain.EvidenceOrder: {Source: domain.EvidenceOrder, Ref: "ord_1", Found: false},
		},
		Audit: []domain.AuditEvent{
			{Kind: domain.AuditLLMCall, Step: "analyze", Model: "m", PromptVersion: "analyze.v1", InputTokens: 900, OutputTokens: 120, OK: true, Latency: 2 * time.Second},
			{Kind: domain.AuditToolCall, Step: "gather_evidence", Name: "get_order", OK: true, Detail: map[string]any{"id": "ord_1", "found": false}},
		},
		Usage: domain.Usage{LLMCalls: 1, ToolCalls: 1, InputTokens: 900, OutputTokens: 120},
	}
}

func newTestServer(t *testing.T, secret string) (*httptest.Server, *fakeDisputes) {
	t.Helper()
	fake := &fakeDisputes{view: readyView()}
	srv, err := New(fake, Config{WebhookSecret: secret, Pricing: Pricing{InputPerMTok: 1, OutputPerMTok: 2}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, fake
}

func TestWebhookSignature(t *testing.T) {
	ts, fake := newTestServer(t, "s3cret")
	body := `{"case_id":"X"}`

	post := func(sig string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/webhooks/disputes", strings.NewReader(body))
		if sig != "" {
			req.Header.Set("X-Signature", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if r := post(""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", r.StatusCode)
	}
	if r := post(intake.Sign("wrong", []byte(body))); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", r.StatusCode)
	}
	if r := post(intake.Sign("s3cret", []byte(body))); r.StatusCode != http.StatusAccepted {
		t.Errorf("signed: %d", r.StatusCode)
	}
	if r := post(intake.Sign("s3cret", []byte(body))); r.StatusCode != http.StatusOK {
		t.Errorf("duplicate delivery should be 200, got %d", r.StatusCode)
	}
	if fake.submitted[0].Source != domain.SourceWebhook {
		t.Errorf("source = %s", fake.submitted[0].Source)
	}
}

func TestAPISubmitValidation(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, err := http.Post(ts.URL+"/api/v1/disputes", "text/plain", strings.NewReader("invalid notice"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIGet(t *testing.T) {
	ts, fake := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/api/v1/disputes/" + fake.view.Dispute.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got detailJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Recommendation != "fight" || got.Category != "fraud" || !got.Evidence["transaction"].Found {
		t.Errorf("unexpected body: %+v", got)
	}
	if got.Usage.CostUSD <= 0 || got.ProcessingMS <= 0 {
		t.Errorf("usage = %+v, processing = %d", got.Usage, got.ProcessingMS)
	}

	resp2, _ := http.Get(ts.URL + "/api/v1/disputes/not-a-uuid")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("bad id: %d", resp2.StatusCode)
	}
}

func TestPagesRender(t *testing.T) {
	ts, fake := newTestServer(t, "")
	id := fake.view.Dispute.ID.String()

	for _, path := range []string{"/disputes", "/disputes/table", "/disputes/new", "/disputes/" + id} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d: %s", path, resp.StatusCode, b)
		}
		if strings.Contains(string(b), "<script>") && path == "/disputes/"+id {
			t.Error("claim text was not escaped")
		}
	}

	resp, _ := http.Get(ts.URL + "/disputes/" + id)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	for _, want := range []string{"VDR-26-0004817", `transaction.three_ds.status = &#34;authenticated&#34;`, "The rule wins", "order record not available", "Approve"} {
		if !strings.Contains(page, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

func TestProgressAsksForRefreshWhenDone(t *testing.T) {
	ts, fake := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/disputes/" + fake.view.Dispute.ID.String() + "/progress")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Error("finished dispute should trigger a refresh")
	}
}

func TestReviewForm(t *testing.T) {
	ts, fake := newTestServer(t, "")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := ts.URL + "/disputes/" + fake.view.Dispute.ID.String() + "/review"

	resp, err := client.PostForm(target, url.Values{"action": {"approve"}, "decision": {"fight"}, "body": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(b), "reviewer name is required") {
		t.Errorf("missing reviewer: %d", resp.StatusCode)
	}

	resp, err = client.PostForm(target, url.Values{"action": {"approve"}, "reviewer": {"Ana"}, "decision": {"fight"}, "body": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || fake.approved == nil || fake.approved.Reviewer != "Ana" {
		t.Errorf("approve: %d %+v", resp.StatusCode, fake.approved)
	}
}

func TestDownloads(t *testing.T) {
	ts, fake := newTestServer(t, "")
	base := ts.URL + "/disputes/" + fake.view.Dispute.ID.String()
	for ext, ctype := range map[string]string{"md": "text/markdown", "pdf": "application/pdf"} {
		resp, err := http.Get(base + "/package." + ext)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", ext, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(resp.Header.Get("Content-Disposition"), "dispute-VDR-26-0004817."+ext) {
			t.Errorf("%s: disposition %s", ext, resp.Header.Get("Content-Disposition"))
		}
	}
}
