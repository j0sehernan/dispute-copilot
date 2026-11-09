// Package app has the small pieces of wiring the binaries share.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/j0sehernan/dispute-copilot/internal/config"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/llm/ollama"
	"github.com/j0sehernan/dispute-copilot/internal/llm/openai"
	"github.com/j0sehernan/dispute-copilot/internal/redact"
)

// Logger writes JSON logs with personal data scrubbed.
func Logger(level string, service string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl, ReplaceAttr: redact.ReplaceAttr})
	return slog.New(h).With("service", service)
}

// SignalContext is cancelled on SIGINT or SIGTERM.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Models builds the chat client and the embedder from the config.
func Models(cfg config.Config) (llm.Client, llm.Embedder, error) {
	var (
		client llm.Client
		oai    *openai.Client
		oll    *ollama.Client
	)
	switch cfg.LLMProvider {
	case "openai":
		oai = openai.New(openai.Config{
			BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel,
			EmbedModel: cfg.EmbedModel, JSONMode: cfg.JSONMode, Timeout: cfg.LLMTimeout,
		})
		client = oai
	case "ollama":
		oll = ollama.New(ollama.Config{
			BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, EmbedModel: cfg.EmbedModel, Timeout: cfg.LLMTimeout,
		})
		client = oll
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider %q", cfg.LLMProvider)
	}

	var emb llm.Embedder
	switch cfg.EmbedProvider {
	case "hash":
		emb = llm.HashEmbedder{}
	case "openai":
		if oai == nil {
			return nil, nil, fmt.Errorf("EMBED_PROVIDER=openai needs LLM_PROVIDER=openai")
		}
		emb = oai.Embedder()
	case "ollama":
		if oll == nil {
			// Chat on an OpenAI-compatible API, embeddings from a local Ollama.
			oll = ollama.New(ollama.Config{BaseURL: envOr("OLLAMA_URL", "http://localhost:11434"), EmbedModel: cfg.EmbedModel, Timeout: cfg.LLMTimeout})
		}
		emb = oll.Embedder()
	default:
		return nil, nil, fmt.Errorf("unknown embedding provider %q", cfg.EmbedProvider)
	}
	return client, emb, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
