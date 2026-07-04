package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"behemoth/internal/boss"
	"github.com/go-chi/chi/v5"
)

// Server holds handler dependencies.
type Server struct {
	svc     *boss.Service
	ready   *atomic.Bool
	log     *slog.Logger
	metrics *Metrics
}

func NewServer(svc *boss.Service, ready *atomic.Bool, log *slog.Logger, m *Metrics) *Server {
	return &Server{svc: svc, ready: ready, log: log, metrics: m}
}

type damageRequest struct {
	PlayerID     string `json:"player_id"`
	BossID       string `json:"boss_id"`
	DamageAmount int64  `json:"damage_amount"`
}

type claimRequest struct {
	PlayerID string `json:"player_id"`
	BossID   string `json:"boss_id"`
}

func (s *Server) handleDamage(w http.ResponseWriter, r *http.Request) {
	var req damageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.PlayerID == "" || req.BossID == "" {
		writeError(w, http.StatusBadRequest, "player_id and boss_id are required")
		return
	}

	res, err := s.svc.Damage(r.Context(), req.BossID, req.PlayerID, req.DamageAmount)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.metrics.DamageApplied.Add(float64(res.Applied))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGetBoss(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "boss id is required")
		return
	}
	view, err := s.svc.Get(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.PlayerID == "" || req.BossID == "" {
		writeError(w, http.StatusBadRequest, "player_id and boss_id are required")
		return
	}
	res, err := s.svc.Claim(r.Context(), req.BossID, req.PlayerID)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	// Always 200: idempotent; the already_claimed flag distinguishes replays.
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, "rehydrating cache")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// writeDomainError maps domain errors to HTTP status codes.
func (s *Server) writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, boss.ErrInvalidDamage):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, boss.ErrBossNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, boss.ErrBossAlreadyDefeated), errors.Is(err, boss.ErrBossNotDefeated):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, boss.ErrNoContribution):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, boss.ErrOverloaded):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.log.Error("internal error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
