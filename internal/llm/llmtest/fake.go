// Package llmtest has a scripted llm.Client for tests.
package llmtest

import (
	"context"
	"sync"

	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

// Func decides the reply for one request. Tests usually switch on
// req.Schema.Name or on the tools offered.
type Func func(req llm.Request) (llm.Message, error)

// Client is a fake model. It records every request it receives.
type Client struct {
	Respond Func

	mu       sync.Mutex
	requests []llm.Request
}

func (c *Client) Model() string { return "fake-model" }

func (c *Client) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	msg, err := c.Respond(req)
	if err != nil {
		return nil, err
	}
	msg.Role = llm.RoleAssistant
	return &llm.Response{
		Message: msg,
		Model:   c.Model(),
		Usage:   llm.Usage{InputTokens: 100, OutputTokens: 20},
	}, nil
}

func (c *Client) Requests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.Request(nil), c.requests...)
}

// Text replies with plain content.
func Text(s string) (llm.Message, error) { return llm.Message{Content: s}, nil }
