package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// observe records latency + status metrics and a structured access log line.
func observe(log *slog.Logger, m *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			elapsed := time.Since(start)

			// Route pattern, not raw path, to bound label cardinality.
			route := r.URL.Path
			if pat := chi.RouteContext(r.Context()).RoutePattern(); pat != "" {
				route = pat
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			m.Duration.WithLabelValues(route).Observe(elapsed.Seconds())
			m.Requests.WithLabelValues(route, r.Method, http.StatusText(status)).Inc()

			log.Info("request",
				"method", r.Method,
				"route", route,
				"status", status,
				"bytes", ww.BytesWritten(),
				"dur_ms", elapsed.Milliseconds(),
			)
		})
	}
}
