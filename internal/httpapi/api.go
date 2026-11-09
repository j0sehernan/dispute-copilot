package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
	"github.com/j0sehernan/dispute-copilot/internal/service"
)

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.cfg.WebhookSecret != "" && !intake.VerifySignature(s.cfg.WebhookSecret, body, r.Header.Get("X-Signature")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}
	if intake.Detect(body) != domain.InputJSON {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "webhook body must be JSON"})
		return
	}
	s.submit(w, r, domain.SourceWebhook, body)
}

// apiSubmit accepts a notice as raw text, JSON or PDF in the request body.
func (s *Server) apiSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.submit(w, r, domain.SourceUpload, body)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, src domain.Source, body []byte) {
	res, err := s.disputes.Submit(r.Context(), service.Submission{
		Source:         src,
		Content:        body,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusAccepted
	if !res.Created {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/api/v1/disputes/"+res.ID.String())
	writeJSON(w, status, map[string]any{
		"id":        res.ID,
		"duplicate": !res.Created,
		"status":    "/api/v1/disputes/" + res.ID.String(),
		"review":    "/disputes/" + res.ID.String(),
	})
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, intake.MaxUploadBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("%w: body is larger than 5 MB", domain.ErrInvalidInput)
		}
		return nil, err
	}
	return body, nil
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	list, err := s.disputes.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]disputeSummary, 0, len(list))
	for _, d := range list {
		out = append(out, summarize(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"disputes": out})
}

func (s *Server) apiGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.detailJSON(v))
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events := make([]auditJSON, 0, len(v.Audit))
	for _, ev := range v.Audit {
		events = append(events, auditJSON{
			At: ev.CreatedAt, Step: ev.Step, Kind: string(ev.Kind), Name: ev.Name,
			PromptVersion: ev.PromptVersion, Model: ev.Model,
			InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens,
			LatencyMS: ev.Latency.Milliseconds(), OK: ev.OK, Detail: ev.Detail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) view(r *http.Request) (*service.View, error) {
	id, err := disputeID(r)
	if err != nil {
		return nil, err
	}
	return s.disputes.Get(r.Context(), id)
}

type disputeSummary struct {
	ID             string         `json:"id"`
	Status         string         `json:"status"`
	CaseID         string         `json:"case_id,omitempty"`
	Network        domain.Network `json:"network,omitempty"`
	ReasonCode     string         `json:"reason_code,omitempty"`
	Category       string         `json:"category,omitempty"`
	Amount         string         `json:"amount,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
	Decision       string         `json:"decision,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	ReadyAt        *time.Time     `json:"ready_at,omitempty"`
}

func summarize(d *domain.Dispute) disputeSummary {
	s := disputeSummary{
		ID:        d.ID.String(),
		Status:    string(d.Status),
		Category:  string(d.Category),
		Decision:  string(d.Decision),
		CreatedAt: d.CreatedAt,
		ReadyAt:   d.ReadyAt,
	}
	if det := d.Details; det != nil {
		s.CaseID, s.Network, s.ReasonCode, s.Amount = det.CaseID, det.Network, det.ReasonCode, det.Amount.String()
	}
	if d.Analysis != nil {
		s.Recommendation = string(d.Analysis.Recommendation)
	}
	return s
}

type evidenceJSON struct {
	Ref   string          `json:"ref"`
	Found bool            `json:"found"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type usageJSON struct {
	LLMCalls     int     `json:"llm_calls"`
	ToolCalls    int     `json:"tool_calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	LLMLatencyMS int64   `json:"llm_latency_ms"`
	CostUSD      float64 `json:"cost_usd"`
}

type detailJSON struct {
	disputeSummary
	Source        string                  `json:"source"`
	InputKind     string                  `json:"input_kind"`
	RuleID        string                  `json:"rule_id,omitempty"`
	FailureReason string                  `json:"failure_reason,omitempty"`
	ProcessingMS  int64                   `json:"processing_ms,omitempty"`
	Analysis      *domain.Analysis        `json:"analysis,omitempty"`
	Letter        map[string]any          `json:"letter"`
	Evidence      map[string]evidenceJSON `json:"evidence"`
	Usage         usageJSON               `json:"usage"`
}

func (s *Server) detailJSON(v *service.View) detailJSON {
	d := v.Dispute
	out := detailJSON{
		disputeSummary: summarize(d),
		Source:         string(d.Source),
		InputKind:      string(d.InputKind),
		RuleID:         d.RuleID,
		FailureReason:  d.FailureReason,
		Analysis:       d.Analysis,
		Letter:         map[string]any{"subject": d.Letter.Subject, "body": d.Letter.Body, "edited": d.Letter.Edited},
		Evidence:       map[string]evidenceJSON{},
		Usage: usageJSON{
			LLMCalls:     v.Usage.LLMCalls,
			ToolCalls:    v.Usage.ToolCalls,
			InputTokens:  v.Usage.InputTokens,
			OutputTokens: v.Usage.OutputTokens,
			LLMLatencyMS: v.Usage.LLMLatency.Milliseconds(),
			CostUSD:      s.cfg.Pricing.Cost(v.Usage),
		},
	}
	if d.ReadyAt != nil {
		out.ProcessingMS = d.ReadyAt.Sub(d.CreatedAt).Milliseconds()
	}
	for src, it := range v.Evidence {
		out.Evidence[string(src)] = evidenceJSON{Ref: it.Ref, Found: it.Found, Data: it.Data}
	}
	return out
}

type auditJSON struct {
	At            time.Time      `json:"at"`
	Step          string         `json:"step"`
	Kind          string         `json:"kind"`
	Name          string         `json:"name,omitempty"`
	PromptVersion string         `json:"prompt_version,omitempty"`
	Model         string         `json:"model,omitempty"`
	InputTokens   int            `json:"input_tokens,omitempty"`
	OutputTokens  int            `json:"output_tokens,omitempty"`
	LatencyMS     int64          `json:"latency_ms,omitempty"`
	OK            bool           `json:"ok"`
	Detail        map[string]any `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusFor(err)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "route", r.URL.Path, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": publicMessage(err)})
}
