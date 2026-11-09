package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

func TestCompleteMapsToolsAndUsage(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing auth header")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{
			"model": "gpt-test",
			"choices": [{"message": {"content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_transaction", "arguments": "{\"transaction_id\":\"txn_1\"}"}}
			]}}],
			"usage": {"prompt_tokens": 120, "completion_tokens": 15}
		}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "gpt-test"})
	resp, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "sys"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c0", Name: "get_order", Arguments: json.RawMessage(`{}`)}}},
			{Role: llm.RoleTool, ToolCallID: "c0", ToolName: "get_order", Content: `{"id":"o1"}`},
		},
		Tools:  []llm.Tool{{Name: "get_transaction", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Schema: &llm.Schema{Name: "x", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_transaction" {
		t.Errorf("tools not sent: %+v", got.Tools)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format = %+v", got.ResponseFormat)
	}
	if got.Messages[1].Content != nil {
		t.Errorf("assistant tool-call message should have null content")
	}
	if got.Messages[2].ToolCallID != "c0" {
		t.Errorf("tool_call_id not sent")
	}

	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_transaction" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if string(resp.Message.ToolCalls[0].Arguments) != `{"transaction_id":"txn_1"}` {
		t.Errorf("arguments = %s", resp.Message.ToolCalls[0].Arguments)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 15 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestCompleteRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "m"})
	c.http.Backoff = 1 // keep the test fast
	resp, err := c.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "ok" || calls.Load() != 2 {
		t.Errorf("content=%q calls=%d", resp.Message.Content, calls.Load())
	}
}

func TestCompleteDoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := New(Config{BaseURL: srv.URL}).Complete(context.Background(), llm.Request{})
	var se *llm.StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, llm.ErrTemporary) {
		t.Error("400 must not be temporary")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestEmbedKeepsInputOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[2]},{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()

	vecs, err := New(Config{BaseURL: srv.URL}).Embedder().Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][0] != 1 || vecs[1][0] != 2 {
		t.Errorf("vecs = %v", vecs)
	}
}
