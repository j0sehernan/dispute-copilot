// Package llm defines the small surface the agent needs from a language
// model: chat with optional tools, structured output and embeddings.
// Providers live in sub-packages.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role    Role
	Content string

	// Set on assistant messages that request tools.
	ToolCalls []ToolCall

	// Set on tool result messages.
	ToolCallID string
	ToolName   string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Tool is a function the model may call. Parameters is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Schema asks the provider for a JSON response that matches it. Providers
// treat it as a hint; callers must still validate the output.
type Schema struct {
	Name   string
	Schema json.RawMessage
}

type Request struct {
	Messages    []Message
	Tools       []Tool
	Schema      *Schema
	Temperature float64
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
}

type Response struct {
	Message Message
	Usage   Usage
	Model   string
	Latency time.Duration
}

type Client interface {
	Complete(ctx context.Context, req Request) (*Response, error)
	Model() string
}

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// ErrTemporary marks failures worth retrying later: rate limits, timeouts,
// 5xx. The job queue uses it to decide between retry and fail.
var ErrTemporary = errors.New("temporary llm failure")

// StatusError is a non-2xx answer from a provider.
type StatusError struct {
	Provider string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: http %d: %s", e.Provider, e.Code, e.Body)
}

func (e *StatusError) Is(target error) bool {
	return target == ErrTemporary && (e.Code == 429 || e.Code >= 500)
}
