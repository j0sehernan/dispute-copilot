// Command worker runs the agent: it takes jobs from the queue and moves
// disputes through extraction, evidence gathering and analysis.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/j0sehernan/dispute-copilot/internal/agent"
	"github.com/j0sehernan/dispute-copilot/internal/app"
	"github.com/j0sehernan/dispute-copilot/internal/config"
	"github.com/j0sehernan/dispute-copilot/internal/pipeline"
	"github.com/j0sehernan/dispute-copilot/internal/psp"
	"github.com/j0sehernan/dispute-copilot/internal/queue"
	"github.com/j0sehernan/dispute-copilot/internal/rules"
	"github.com/j0sehernan/dispute-copilot/internal/store"
)

func main() {
	cfg, err := config.Load()
	log := app.Logger(cfg.LogLevel, "worker")
	if err == nil {
		err = cfg.ValidateLLM()
	}
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	if err := run(cfg, log); err != nil {
		log.Error("worker stopped", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := app.SignalContext()
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	client, embedder, err := app.Models(cfg)
	if err != nil {
		return err
	}
	lib, err := rules.Load()
	if err != nil {
		return err
	}
	retriever := rules.NewRetriever(lib, st, embedder)
	if err := indexRules(ctx, retriever, log); err != nil {
		return err
	}

	recorder := agent.NewMetricsRecorder(st)
	records := psp.New(cfg.MockPSPURL, 10*time.Second)
	p := &pipeline.Pipeline{
		Store:     st,
		Recorder:  recorder,
		Rules:     lib,
		Extractor: agent.NewExtractor(client, recorder),
		Gatherer:  agent.NewGatherer(client, records, recorder),
		Retriever: retriever,
		Analyst:   agent.NewAnalyst(client, recorder),
		Drafter:   agent.NewDrafter(client, recorder),
		Log:       log,
	}

	go serveMetrics(ctx, log)

	w := queue.NewWorker(st, p.Handlers(), log, queue.Options{
		Concurrency: cfg.WorkerConcurrency,
		Lease:       cfg.JobLease,
		Poll:        cfg.PollInterval,
		Wake:        st.ListenJobs(ctx),
		OnFailure:   p.OnFailure,
	})
	log.Info("worker started",
		"provider", cfg.LLMProvider, "model", cfg.LLMModel,
		"embeddings", embedder.Model(), "concurrency", cfg.WorkerConcurrency)
	w.Run(ctx)
	return nil
}

// indexRules embeds the rule library. With Ollama the model may still be
// downloading when the worker starts, so it keeps trying for a while.
func indexRules(ctx context.Context, r *rules.Retriever, log *slog.Logger) error {
	deadline := time.Now().Add(15 * time.Minute)
	for {
		n, err := r.Index(ctx)
		if err == nil {
			log.Info("rule library indexed", "new_chunks", n)
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		log.Warn("indexing rules failed, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func serveMetrics(ctx context.Context, log *slog.Logger) {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("metrics server", "err", err)
	}
}
