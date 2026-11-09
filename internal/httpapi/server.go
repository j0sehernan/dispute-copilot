// Package httpapi serves the JSON API, the webhook and the review UI.
package httpapi

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Disputes interface {
	Submit(ctx context.Context, sub service.Submission) (service.SubmitResult, error)
	Get(ctx context.Context, id uuid.UUID) (*service.View, error)
	List(ctx context.Context) ([]*domain.Dispute, error)
	SaveDraft(ctx context.Context, id uuid.UUID, in service.ReviewInput) error
	Approve(ctx context.Context, id uuid.UUID, in service.ReviewInput) error
}

// Pricing turns token counts into a cost estimate.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

func (p Pricing) Cost(u domain.Usage) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerMTok + float64(u.OutputTokens)/1e6*p.OutputPerMTok
}

type Config struct {
	WebhookSecret string
	Pricing       Pricing
	Ready         func(ctx context.Context) error
}

type Server struct {
	disputes Disputes
	cfg      Config
	log      *slog.Logger
	pages    map[string]*template.Template
}

func New(disputes Disputes, cfg Config, log *slog.Logger) (*Server, error) {
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{disputes: disputes, cfg: cfg, log: log, pages: pages}, nil
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.logRequests)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", s.readyz)
	r.Handle("/metrics", promhttp.Handler())

	static, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))
		r.Post("/webhooks/disputes", s.webhook)
		r.Post("/disputes", s.apiSubmit)
		r.Get("/disputes", s.apiList)
		r.Get("/disputes/{id}", s.apiGet)
		r.Get("/disputes/{id}/audit", s.apiAudit)
	})

	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/disputes", http.StatusFound) })
	r.Get("/disputes", s.pageList)
	r.Get("/disputes/table", s.fragmentTable)
	r.Get("/disputes/new", s.pageNew)
	r.Post("/disputes", s.formSubmit)
	r.Get("/disputes/{id}", s.pageDetail)
	r.Get("/disputes/{id}/progress", s.fragmentProgress)
	r.Post("/disputes/{id}/review", s.formReview)
	r.Get("/disputes/{id}/package.md", s.downloadMarkdown)
	r.Get("/disputes/{id}/package.pdf", s.downloadPDF)
	return r
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready(r.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// logRequests logs method, route pattern and status. It never logs bodies or
// query strings.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "/healthz" || route == "/metrics" || route == "/static/*" {
			return
		}
		s.log.Info("http",
			"method", r.Method,
			"route", route,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

func disputeID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, domain.ErrNotFound
	}
	return id, nil
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrInvalidInput):
		return http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrInvalidState):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// publicMessage hides internal errors from clients.
func publicMessage(err error) string {
	if statusFor(err) == http.StatusInternalServerError {
		return "internal error"
	}
	return err.Error()
}
