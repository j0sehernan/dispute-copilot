package domain

import (
	"time"

	"github.com/google/uuid"
)

type AuditKind string

const (
	AuditLLMCall      AuditKind = "llm_call"
	AuditParse        AuditKind = "parse"
	AuditToolCall     AuditKind = "tool_call"
	AuditGuardrail    AuditKind = "guardrail"
	AuditStatus       AuditKind = "status_change"
	AuditDecision     AuditKind = "decision"
	AuditHumanEdit    AuditKind = "human_edit"
	AuditHumanApprove AuditKind = "human_approval"
)

// AuditEvent is one row of the audit trail. Detail must never hold personal
// data: tool arguments are IDs, and errors are short codes or messages we
// write ourselves.
type AuditEvent struct {
	ID            int64
	DisputeID     uuid.UUID
	Step          string
	Kind          AuditKind
	Name          string
	PromptVersion string
	Model         string
	InputTokens   int
	OutputTokens  int
	Latency       time.Duration
	OK            bool
	Detail        map[string]any
	CreatedAt     time.Time
}

// Usage aggregates the model usage of one dispute.
type Usage struct {
	LLMCalls     int
	ToolCalls    int
	InputTokens  int
	OutputTokens int
	LLMLatency   time.Duration
}
