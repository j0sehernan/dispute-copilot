package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

func TestComplete(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{
			"message": {"role": "assistant", "content": "", "tool_calls": [
				{"function": {"name": "get_shipping", "arguments": {"order_id": "ord_9"}}}
			]},
			"prompt_eval_count": 300, "eval_count": 12
		}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "qwen2.5:7b"})
	resp, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleTool, ToolName: "get_order", Content: "{}"}},
		Schema:   &llm.Schema{Name: "s", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stream {
		t.Error("stream must be false")
	}
	if string(got.Format) != `{"type":"object"}` {
		t.Errorf("format = %s", got.Format)
	}
	if got.Messages[0].ToolName != "get_order" {
		t.Errorf("tool_name not sent")
	}

	tc := resp.Message.ToolCalls
	if len(tc) != 1 || tc[0].Name != "get_shipping" || tc[0].ID == "" {
		t.Fatalf("tool calls = %+v", tc)
	}
	if string(tc[0].Arguments) != `{"order_id": "ord_9"}` {
		t.Errorf("arguments = %s", tc[0].Arguments)
	}
	if resp.Usage.InputTokens != 300 || resp.Usage.OutputTokens != 12 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}
