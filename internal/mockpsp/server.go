// Package mockpsp stands in for the systems a merchant would query during a
// dispute: the payment processor (transactions), the order system, the
// carrier and the CRM. It serves the synthetic fixtures over HTTP so the
// agent talks to it exactly as it would talk to real services.
package mockpsp

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
)

type Server struct {
	transactions map[string]json.RawMessage
	orders       map[string]json.RawMessage
	shipments    map[string]json.RawMessage // by order id
	histories    map[string]json.RawMessage // by customer id

	// Latency adds a random delay up to this value to every response, to
	// make the demo behave a bit more like real network calls.
	Latency time.Duration
}

func New(scenarios []fixtures.Scenario) (*Server, error) {
	s := &Server{
		transactions: map[string]json.RawMessage{},
		orders:       map[string]json.RawMessage{},
		shipments:    map[string]json.RawMessage{},
		histories:    map[string]json.RawMessage{},
	}
	for _, sc := range scenarios {
		if err := index(s.transactions, sc.Data.Transaction, "id"); err != nil {
			return nil, fmt.Errorf("%s: transaction: %w", sc.ID, err)
		}
		if err := index(s.orders, sc.Data.Order, "id"); err != nil {
			return nil, fmt.Errorf("%s: order: %w", sc.ID, err)
		}
		if err := index(s.shipments, sc.Data.Shipping, "order_id"); err != nil {
			return nil, fmt.Errorf("%s: shipping: %w", sc.ID, err)
		}
		if err := index(s.histories, sc.Data.CustomerHistory, "customer_id"); err != nil {
			return nil, fmt.Errorf("%s: customer history: %w", sc.ID, err)
		}
	}
	return s, nil
}

func index(m map[string]json.RawMessage, doc json.RawMessage, key string) error {
	if len(doc) == 0 {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(doc, &fields); err != nil {
		return err
	}
	id, _ := fields[key].(string)
	if id == "" {
		return fmt.Errorf("missing %q", key)
	}
	m[id] = doc
	return nil
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/v1/transactions/{id}", s.serve(s.transactions))
	r.Get("/v1/orders/{id}", s.serve(s.orders))
	r.Get("/v1/orders/{id}/shipment", s.serve(s.shipments))
	r.Get("/v1/customers/{id}/history", s.serve(s.histories))
	return r
}

func (s *Server) serve(m map[string]json.RawMessage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Latency > 0 {
			select {
			case <-time.After(rand.N(s.Latency)):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		doc, ok := m[chi.URLParam(r, "id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		_, _ = w.Write(doc)
	}
}
