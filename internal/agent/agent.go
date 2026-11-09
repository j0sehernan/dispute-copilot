// Package agent contains the LLM steps of the dispute workflow: extraction,
// evidence gathering with tools, analysis and the letter draft. Each step
// validates what the model returns before anything is stored.
package agent

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
)

//go:embed prompts/*.txt schemas/*.json
var assets embed.FS

// ErrInvalidOutput means the model failed validation twice in a row. It is
// not worth retrying the job: the same input will most likely fail again.
var ErrInvalidOutput = errors.New("model output failed validation")

// Recorder stores audit events. Failures to record are the recorder's
// problem; the agent never fails a step because of them.
type Recorder interface {
	Record(ctx context.Context, ev domain.AuditEvent)
}

type Prompt struct {
	Name    string
	Version string
	Text    string
}

// loadPrompt reads a versioned prompt such as "extract.v1". Changing a
// prompt means adding a new file, so the audit log always says which text
// produced an answer.
func loadPrompt(version string) Prompt {
	b, err := assets.ReadFile("prompts/" + version + ".txt")
	if err != nil {
		panic(fmt.Sprintf("agent: prompt %s: %v", version, err))
	}
	name, _, _ := strings.Cut(version, ".")
	return Prompt{Name: name, Version: version, Text: string(b)}
}

type schema struct {
	name     string
	raw      json.RawMessage
	compiled *jsonschema.Schema
}

func loadSchema(name string) schema {
	raw, err := assets.ReadFile("schemas/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("agent: schema %s: %v", name, err))
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	if err := c.AddResource(name+".json", strings.NewReader(string(raw))); err != nil {
		panic(err)
	}
	compiled, err := c.Compile(name + ".json")
	if err != nil {
		panic(fmt.Sprintf("agent: compile schema %s: %v", name, err))
	}
	return schema{name: name, raw: raw, compiled: compiled}
}

func (s schema) validate(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if err := s.compiled.Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(flattenSchemaError(ve))
		}
		return err
	}
	return nil
}

// flattenSchemaError turns the nested validator error into a short list the
// model can act on when we ask it to fix its answer.
func flattenSchemaError(ve *jsonschema.ValidationError) string {
	var msgs []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := e.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			msgs = append(msgs, loc+": "+e.Message)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(msgs) > 8 {
		msgs = append(msgs[:8], fmt.Sprintf("and %d more", len(msgs)-8))
	}
	return strings.Join(msgs, "; ")
}

// structured runs a structured-output call: schema check, then the
// step-specific check, and one retry that shows the model its mistakes.
type structured[T any] struct {
	client   llm.Client
	recorder Recorder
	prompt   Prompt
	schema   schema
	step     string
	// check validates meaning (references, ids) after the schema passed.
	check func(T) error
}

func (s structured[T]) run(ctx context.Context, disputeID uuid.UUID, user string) (T, error) {
	var zero T
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: s.prompt.Text},
		{Role: llm.RoleUser, Content: user},
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := s.client.Complete(ctx, llm.Request{
			Messages:    msgs,
			Schema:      &llm.Schema{Name: s.schema.name, Schema: s.schema.raw},
			Temperature: 0,
		})
		if err != nil {
			s.audit(ctx, disputeID, nil, attempt, err)
			return zero, err
		}

		out, verr := s.parse(resp.Message.Content)
		s.audit(ctx, disputeID, resp, attempt, verr)
		if verr == nil {
			return out, nil
		}
		lastErr = verr
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: resp.Message.Content},
			llm.Message{Role: llm.RoleUser, Content: "Your answer is not valid: " + verr.Error() +
				"\nFix these problems and reply again with the full JSON object only."},
		)
	}
	return zero, fmt.Errorf("%s: %w: %w", s.step, ErrInvalidOutput, lastErr)
}

func (s structured[T]) parse(content string) (T, error) {
	var out T
	data := []byte(stripCodeFence(content))
	if err := s.schema.validate(data); err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if s.check != nil {
		if err := s.check(out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s structured[T]) audit(ctx context.Context, id uuid.UUID, resp *llm.Response, attempt int, err error) {
	ev := domain.AuditEvent{
		DisputeID:     id,
		Step:          s.step,
		Kind:          domain.AuditLLMCall,
		Name:          s.schema.name,
		PromptVersion: s.prompt.Version,
		Model:         s.client.Model(),
		OK:            err == nil,
		Detail:        map[string]any{"attempt": attempt},
	}
	if resp != nil {
		ev.Model = resp.Model
		ev.InputTokens = resp.Usage.InputTokens
		ev.OutputTokens = resp.Usage.OutputTokens
		ev.Latency = resp.Latency
	}
	if err != nil {
		ev.Detail["error"] = truncate(err.Error(), 500)
	}
	s.recorder.Record(ctx, ev)
}

// stripCodeFence tolerates models that wrap JSON in ```json fences even
// when asked not to.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
