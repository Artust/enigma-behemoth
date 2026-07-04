package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Router builds the HTTP handler with middleware and routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)
	r.Use(observe(s.log, s.metrics))
	// Cap request handling time so a slow dependency can't pin a connection.
	r.Use(middleware.Timeout(10 * time.Second))

	r.Post("/damage", s.handleDamage)
	r.Get("/boss/{id}", s.handleGetBoss)
	r.Post("/rewards/claim", s.handleClaim)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.Handler())

	return r
}
