package agent

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

var (
	llmCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "copilot_llm_calls_total",
		Help: "Model calls by step and result.",
	}, []string{"step", "ok"})
	llmTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "copilot_llm_tokens_total",
		Help: "Tokens used, by step and direction.",
	}, []string{"step", "direction"})
	llmLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "copilot_llm_latency_seconds",
		Help:    "Model call latency by step.",
		Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
	}, []string{"step"})
	toolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "copilot_tool_calls_total",
		Help: "Tool calls by tool and result.",
	}, []string{"tool", "ok"})
	guardrails = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "copilot_guardrail_events_total",
		Help: "Guardrail interventions by name.",
	}, []string{"name"})
)

// MetricsRecorder updates Prometheus counters and passes every event on.
type MetricsRecorder struct {
	next Recorder
}

func NewMetricsRecorder(next Recorder) MetricsRecorder { return MetricsRecorder{next: next} }

func (m MetricsRecorder) Record(ctx context.Context, ev domain.AuditEvent) {
	ok := "true"
	if !ev.OK {
		ok = "false"
	}
	switch ev.Kind {
	case domain.AuditLLMCall:
		llmCalls.WithLabelValues(ev.Step, ok).Inc()
		llmTokens.WithLabelValues(ev.Step, "input").Add(float64(ev.InputTokens))
		llmTokens.WithLabelValues(ev.Step, "output").Add(float64(ev.OutputTokens))
		if ev.Latency > 0 {
			llmLatency.WithLabelValues(ev.Step).Observe(ev.Latency.Seconds())
		}
	case domain.AuditToolCall:
		toolCalls.WithLabelValues(ev.Name, ok).Inc()
	case domain.AuditGuardrail:
		name := ev.Name
		if r, _ := ev.Detail["refused"].(string); r != "" {
			name = r
		}
		guardrails.WithLabelValues(name).Inc()
	}
	m.next.Record(ctx, ev)
}
