// Package config reads settings from environment variables. Every binary
// loads the same struct and only uses what it needs.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	LogLevel    string

	// LLM
	LLMProvider string // openai | ollama
	LLMModel    string
	LLMBaseURL  string
	LLMAPIKey   string
	LLMTimeout  time.Duration
	// JSONMode is "json_schema" (default) or "json_object" for
	// OpenAI-compatible servers that do not support schemas.
	JSONMode string

	EmbedProvider string // openai | ollama | hash
	EmbedModel    string

	// Prices in USD per million tokens, used only for the cost estimate.
	PriceInputPerMTok  float64
	PriceOutputPerMTok float64

	// Services
	MockPSPURL    string
	WebhookSecret string

	// Worker
	WorkerConcurrency int
	JobLease          time.Duration
	PollInterval      time.Duration
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
		DatabaseURL:       env("DATABASE_URL", "postgres://copilot:copilot@localhost:5432/copilot?sslmode=disable"),
		LogLevel:          env("LOG_LEVEL", "info"),
		LLMProvider:       env("LLM_PROVIDER", ""),
		LLMModel:          env("LLM_MODEL", ""),
		LLMBaseURL:        env("LLM_BASE_URL", ""),
		LLMAPIKey:         firstNonEmpty(os.Getenv("LLM_API_KEY"), os.Getenv("OPENAI_API_KEY")),
		JSONMode:          env("LLM_JSON_MODE", "json_schema"),
		EmbedProvider:     env("EMBED_PROVIDER", ""),
		EmbedModel:        env("EMBED_MODEL", ""),
		MockPSPURL:        env("MOCK_PSP_URL", "http://localhost:8081"),
		WebhookSecret:     os.Getenv("WEBHOOK_SECRET"),
		WorkerConcurrency: 4,
	}

	var errs []error
	var err error
	if c.LLMTimeout, err = duration("LLM_TIMEOUT", 3*time.Minute); err != nil {
		errs = append(errs, err)
	}
	if c.JobLease, err = duration("JOB_LEASE", 15*time.Minute); err != nil {
		errs = append(errs, err)
	}
	if c.PollInterval, err = duration("POLL_INTERVAL", 2*time.Second); err != nil {
		errs = append(errs, err)
	}
	if v := os.Getenv("WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY must be a positive integer, got %q", v))
		}
		c.WorkerConcurrency = n
	}

	if c.LLMProvider == "" && c.LLMAPIKey != "" {
		c.LLMProvider = "openai"
	}
	switch c.LLMProvider {
	case "openai":
		c.LLMBaseURL = firstNonEmpty(c.LLMBaseURL, "https://api.openai.com/v1")
		c.LLMModel = firstNonEmpty(c.LLMModel, "gpt-4.1-mini")
		c.EmbedModel = firstNonEmpty(c.EmbedModel, "text-embedding-3-small")
	case "ollama":
		c.LLMBaseURL = firstNonEmpty(c.LLMBaseURL, os.Getenv("OLLAMA_URL"), "http://localhost:11434")
		c.LLMModel = firstNonEmpty(c.LLMModel, "qwen2.5:7b")
		c.EmbedModel = firstNonEmpty(c.EmbedModel, "nomic-embed-text")
	}
	c.EmbedProvider = firstNonEmpty(c.EmbedProvider, c.LLMProvider)
	if c.EmbedProvider == "hash" {
		c.EmbedModel = "hash-256"
	}

	in, out := defaultPrice(c.LLMProvider, c.LLMModel)
	if c.PriceInputPerMTok, err = float("LLM_PRICE_INPUT_PER_MTOK", in); err != nil {
		errs = append(errs, err)
	}
	if c.PriceOutputPerMTok, err = float("LLM_PRICE_OUTPUT_PER_MTOK", out); err != nil {
		errs = append(errs, err)
	}

	return c, errors.Join(errs...)
}

// ValidateLLM is called by the binaries that talk to a model (worker, eval
// does not). The API can run without a key.
func (c Config) ValidateLLM() error {
	switch c.LLMProvider {
	case "":
		return errors.New("no LLM configured: set OPENAI_API_KEY, or LLM_PROVIDER=ollama")
	case "openai":
		if c.LLMAPIKey == "" {
			return errors.New("LLM_PROVIDER=openai needs OPENAI_API_KEY (or LLM_API_KEY)")
		}
	case "ollama":
	default:
		return fmt.Errorf("unknown LLM_PROVIDER %q (use openai or ollama)", c.LLMProvider)
	}
	switch c.EmbedProvider {
	case "openai", "ollama", "hash":
	default:
		return fmt.Errorf("unknown EMBED_PROVIDER %q (use openai, ollama or hash)", c.EmbedProvider)
	}
	if c.JSONMode != "json_schema" && c.JSONMode != "json_object" {
		return fmt.Errorf("LLM_JSON_MODE must be json_schema or json_object, got %q", c.JSONMode)
	}
	return nil
}

// defaultPrice knows a few public list prices so the eval can print a cost
// estimate out of the box. Local models cost nothing per token.
func defaultPrice(provider, model string) (in, out float64) {
	if provider == "ollama" {
		return 0, 0
	}
	switch model {
	case "gpt-4o-mini":
		return 0.15, 0.60
	case "gpt-4.1-mini":
		return 0.40, 1.60
	case "gpt-4.1-nano":
		return 0.10, 0.40
	case "gpt-4.1":
		return 2.00, 8.00
	case "gpt-4o":
		return 2.50, 10.00
	}
	return 0, 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func duration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func float(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}
