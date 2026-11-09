// Package apiclient is a small client for the Dispute Copilot API, used by
// the seed and eval commands.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
)

type Client struct {
	BaseURL       string
	WebhookSecret string
	HTTP          *http.Client
}

func New(baseURL, secret string) *Client {
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		WebhookSecret: secret,
		HTTP:          &http.Client{Timeout: 30 * time.Second},
	}
}

// WaitReady blocks until the API answers /readyz or ctx ends.
func (c *Client) WaitReady(ctx context.Context) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/readyz", nil)
		resp, err := c.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("api not ready at %s: %w", c.BaseURL, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

type SubmitResponse struct {
	ID        string `json:"id"`
	Duplicate bool   `json:"duplicate"`
}

// Submit sends a scenario the way it would arrive in real life: JSON
// payloads through the signed webhook, text notices as an upload.
func (c *Client) Submit(ctx context.Context, sc fixtures.Scenario, idempotencyKey string) (SubmitResponse, error) {
	var (
		path  = "/api/v1/disputes"
		body  []byte
		ctype = "text/plain; charset=utf-8"
	)
	if sc.Input.Kind == "json" {
		path, body, ctype = "/api/v1/webhooks/disputes", sc.Input.Payload, "application/json"
	} else {
		body = []byte(sc.Input.Text)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return SubmitResponse{}, err
	}
	req.Header.Set("Content-Type", ctype)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if c.WebhookSecret != "" {
		req.Header.Set("X-Signature", intake.Sign(c.WebhookSecret, body))
	}

	var out SubmitResponse
	if err := c.do(req, &out); err != nil {
		return SubmitResponse{}, fmt.Errorf("%s: %w", sc.ID, err)
	}
	return out, nil
}

type Dispute struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	CaseID         string `json:"case_id"`
	Category       string `json:"category"`
	Recommendation string `json:"recommendation"`
	RuleID         string `json:"rule_id"`
	FailureReason  string `json:"failure_reason"`
	ProcessingMS   int64  `json:"processing_ms"`
	Analysis       *struct {
		PolicyOverride bool `json:"policy_override"`
	} `json:"analysis"`
	Evidence map[string]struct {
		Found bool `json:"found"`
	} `json:"evidence"`
	Usage struct {
		LLMCalls     int     `json:"llm_calls"`
		ToolCalls    int     `json:"tool_calls"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		LLMLatencyMS int64   `json:"llm_latency_ms"`
		CostUSD      float64 `json:"cost_usd"`
	} `json:"usage"`
}

func (d Dispute) Done() bool {
	return d.Status == "ready_for_review" || d.Status == "approved" || d.Status == "failed"
}

func (c *Client) Get(ctx context.Context, id string) (Dispute, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/disputes/"+id, nil)
	if err != nil {
		return Dispute{}, err
	}
	var d Dispute
	return d, c.do(req, &d)
}

type AuditEvent struct {
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Model  string         `json:"model"`
	Detail map[string]any `json:"detail"`
}

func (c *Client) Audit(ctx context.Context, id string) ([]AuditEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/disputes/"+id+"/audit", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Events []AuditEvent `json:"events"`
	}
	return out.Events, c.do(req, &out)
}

// Wait polls until the dispute leaves the processing states.
func (c *Client) Wait(ctx context.Context, id string, every time.Duration) (Dispute, error) {
	for {
		d, err := c.Get(ctx, id)
		if err == nil && d.Done() {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(every):
		}
	}
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
