// Command mockpsp serves the synthetic transactions, orders, shipments and
// customer histories the agent reads as evidence.
package main

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/app"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
	"github.com/j0sehernan/dispute-copilot/internal/mockpsp"
)

func main() {
	log := app.Logger(os.Getenv("LOG_LEVEL"), "mockpsp")

	scenarios, err := fixtures.Load()
	if err != nil {
		log.Error("fixtures", "err", err)
		os.Exit(1)
	}
	srv, err := mockpsp.New(scenarios)
	if err != nil {
		log.Error("index fixtures", "err", err)
		os.Exit(1)
	}
	if v := os.Getenv("MOCK_LATENCY"); v != "" {
		if srv.Latency, err = time.ParseDuration(v); err != nil {
			log.Error("MOCK_LATENCY", "err", err)
			os.Exit(1)
		}
	}

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	log.Info("listening", "addr", addr, "scenarios", len(scenarios))
	s := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
