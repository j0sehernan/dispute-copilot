// Package openai talks to any server that implements the OpenAI chat
// completions and embeddings APIs (OpenAI, Azure-style gateways, vLLM,
// OpenRouter, Groq, ...). It uses plain net/http on purpose: we need four
// fields of the API, not an SDK.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	EmbedModel string
	// JSONMode is "json_schema" or "json_object".
	JSONMode string
	Timeout  time.Duration
}

type Client struct {
	cfg  Config
	http *llm.HTTPDoer
}

func New(cfg Config) *Client {
	if cfg.JSONMode == "" {
		cfg.JSONMode = "json_schema"
	}
	return &Client{
		cfg: cfg,
		http: &llm.HTTPDoer{
			Client:   &http.Client{Timeout: cfg.Timeout},
			Provider: "openai",
			Headers:  map[string]string{"Authorization": "Bearer " + cfg.APIKey},
			Attempts: 3,
		},
	}
}

func (c *Client) Model() string { return c.cfg.Model }

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Tools          []chatTool      `json:"tools,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   *string    `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *Client) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := chatRequest{
		Model:       c.cfg.Model,
		Messages:    toChatMessages(req.Messages),
		Temperature: req.Temperature,
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, chatTool{
			Type:     "function",
			Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	if req.Schema != nil {
		if c.cfg.JSONMode == "json_object" {
			body.ResponseFormat = &responseFormat{Type: "json_object"}
		} else {
			// strict=false: strict mode rejects optional fields, and we
			// validate the result ourselves anyway.
			body.ResponseFormat = &responseFormat{
				Type:       "json_schema",
				JSONSchema: &jsonSchema{Name: req.Schema.Name, Schema: req.Schema.Schema},
			}
		}
	}

	start := time.Now()
	var out chatResponse
	if err := c.http.PostJSON(ctx, strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("openai: response has no choices")
	}

	choice := out.Choices[0].Message
	msg := llm.Message{Role: llm.RoleAssistant}
	if choice.Content != nil {
		msg.Content = *choice.Content
	}
	for _, tc := range choice.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return &llm.Response{
		Message: msg,
		Model:   firstNonEmpty(out.Model, c.cfg.Model),
		Usage:   llm.Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
		Latency: time.Since(start),
	}, nil
}

func toChatMessages(msgs []llm.Message) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := chatMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			content := m.Content
			cm.Content = &content
		}
		for _, tc := range m.ToolCalls {
			call := toolCall{ID: tc.ID, Type: "function"}
			call.Function.Name = tc.Name
			call.Function.Arguments = string(tc.Arguments)
			cm.ToolCalls = append(cm.ToolCalls, call)
		}
		out = append(out, cm)
	}
	return out
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embedder returns an llm.Embedder backed by the /embeddings endpoint.
func (c *Client) Embedder() llm.Embedder { return embedder{c} }

type embedder struct{ c *Client }

func (e embedder) Model() string { return e.c.cfg.EmbedModel }

func (e embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var out embedResponse
	url := strings.TrimRight(e.c.cfg.BaseURL, "/") + "/embeddings"
	if err := e.c.http.PostJSON(ctx, url, embedRequest{Model: e.c.cfg.EmbedModel, Input: texts}, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, errors.New("openai: embeddings count does not match input")
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, errors.New("openai: embedding index out of range")
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
