// Command api serves the review UI, the JSON API and the dispute webhook.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/app"
	"github.com/j0sehernan/dispute-copilot/internal/config"
	"github.com/j0sehernan/dispute-copilot/internal/httpapi"
	"github.com/j0sehernan/dispute-copilot/internal/service"
	"github.com/j0sehernan/dispute-copilot/internal/store"
)

func main() {
	// The runtime image has no shell or curl; the container health check
	// calls the binary itself.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.Load()
	log := app.Logger(cfg.LogLevel, "api")
	if err == nil {
		err = run(cfg, log)
	}
	if err != nil {
		log.Error("api stopped", "err", err)
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
		return fmt.Errorf("migrate: %w", err)
	}

	srv, err := httpapi.New(service.NewDisputes(st), httpapi.Config{
		WebhookSecret: cfg.WebhookSecret,
		Pricing:       httpapi.Pricing{InputPerMTok: cfg.PriceInputPerMTok, OutputPerMTok: cfg.PriceOutputPerMTok},
		Ready:         st.Ping,
	}, log)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", cfg.HTTPAddr, "webhook_signed", cfg.WebhookSecret != "")
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthcheck() int {
	port := "8080"
	if _, p, err := net.SplitHostPort(os.Getenv("HTTP_ADDR")); err == nil && p != "" {
		port = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 1
	}
	return 0
}
