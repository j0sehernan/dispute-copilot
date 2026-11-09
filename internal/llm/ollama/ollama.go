// Package ollama uses Ollama's native /api/chat and /api/embed endpoints, so
// the project runs with a local model and no API key.
package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

type Config struct {
	BaseURL    string
	Model      string
	EmbedModel string
	Timeout    time.Duration
}

type Client struct {
	cfg  Config
	http *llm.HTTPDoer
}

func New(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		http: &llm.HTTPDoer{
			Client:   &http.Client{Timeout: cfg.Timeout},
			Provider: "ollama",
			Attempts: 2,
		},
	}
}

func (c *Client) Model() string { return c.cfg.Model }

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []message       `json:"messages"`
	Tools    []tool          `json:"tools,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatResponse struct {
	Model           string  `json:"model"`
	Message         message `json:"message"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
}

func (c *Client) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := chatRequest{
		Model:   c.cfg.Model,
		Stream:  false,
		Options: map[string]any{"temperature": req.Temperature},
	}
	for _, m := range req.Messages {
		msg := message{Role: string(m.Role), Content: m.Content, ToolName: m.ToolName}
		for _, tc := range m.ToolCalls {
			var call toolCall
			call.Function.Name = tc.Name
			call.Function.Arguments = tc.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
		body.Messages = append(body.Messages, msg)
	}
	for _, t := range req.Tools {
		var tl tool
		tl.Type = "function"
		tl.Function.Name = t.Name
		tl.Function.Description = t.Description
		tl.Function.Parameters = t.Parameters
		body.Tools = append(body.Tools, tl)
	}
	if req.Schema != nil {
		body.Format = req.Schema.Schema
	}

	start := time.Now()
	var out chatResponse
	if err := c.http.PostJSON(ctx, strings.TrimRight(c.cfg.BaseURL, "/")+"/api/chat", body, &out); err != nil {
		return nil, err
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: out.Message.Content}
	for i, tc := range out.Message.ToolCalls {
		args := tc.Function.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		// Ollama does not return call ids; make stable ones so the
		// conversation can reference them.
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:        fmt.Sprintf("call_%d_%s", i, tc.Function.Name),
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}
	return &llm.Response{
		Message: msg,
		Model:   c.cfg.Model,
		Usage:   llm.Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount},
		Latency: time.Since(start),
	}, nil
}

func (c *Client) Embedder() llm.Embedder { return embedder{c} }

type embedder struct{ c *Client }

func (e embedder) Model() string { return e.c.cfg.EmbedModel }

func (e embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	in := map[string]any{"model": e.c.cfg.EmbedModel, "input": texts}
	if err := e.c.http.PostJSON(ctx, strings.TrimRight(e.c.cfg.BaseURL, "/")+"/api/embed", in, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, errors.New("ollama: embeddings count does not match input")
	}
	return out.Embeddings, nil
}
