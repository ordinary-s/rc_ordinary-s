package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/config"
	"github.com/ordinary-s/rc_ordinary-s/internal/domain"
	"github.com/ordinary-s/rc_ordinary-s/internal/notification"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type Server struct {
	cfg           config.Config
	store         *store.Store
	notifications *notification.Service
	logger        *slog.Logger
	mqReady       func(context.Context) error
}

func New(cfg config.Config, s *store.Store, notifications *notification.Service, logger *slog.Logger, mqReady func(context.Context) error) *Server {
	return &Server{cfg: cfg, store: s, notifications: notifications, logger: logger, mqReady: mqReady}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /api/v1/notifications", s.createNotification)
	mux.HandleFunc("GET /api/v1/notifications/{id}", s.getNotification)
	mux.HandleFunc("GET /api/v1/notifications", s.listNotifications)
	mux.HandleFunc("PATCH /api/v1/admin/destinations/{id}/fast-fail", s.setFastFail)
	mux.HandleFunc("POST /api/v1/admin/notifications/{id}/resolve", s.resolveNotification)
	return s.requestLimits(mux)
}

func (s *Server) requestLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.ContentLength > s.cfg.MaxBodyBytes+int64(s.cfg.MaxHeaderBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "mysql unavailable")
		return
	}
	if err := s.mqReady(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "rocketmq unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) createNotification(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authenticateClient(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes+int64(s.cfg.MaxHeaderBytes)+32*1024)
	var input notification.Input
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid notification request")
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is required and limited to 128 bytes")
		return
	}
	traceID := r.Header.Get("X-Trace-ID")
	if !validToken(traceID, 64) {
		var err error
		traceID, err = notification.NewID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not create trace id")
			return
		}
	}
	n, created, err := s.notifications.Accept(r.Context(), clientID, idempotencyKey, traceID, input, s.cfg.MaxBodyBytes, s.cfg.MaxHeaderBytes, s.cfg.TaskTTL)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency key was used with different content")
		return
	}
	if errors.Is(err, notification.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, "invalid notification request")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "destination not found")
		return
	}
	if err != nil {
		s.logger.Error("notification acceptance failed", "trace_id", traceID, "code", "ACCEPT_FAILED", "error_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "notification could not be accepted")
		return
	}
	s.logger.Info("notification accepted", "trace_id", n.TraceID, "task_id", n.ID, "destination_id", n.DestinationID, "result", "ACCEPTED", "created", created)
	writeJSON(w, http.StatusAccepted, map[string]any{"task_id": n.ID, "trace_id": n.TraceID, "status": n.Status, "created": created})
}

func (s *Server) getNotification(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authenticateClient(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	n, err := s.store.GetNotification(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && n.ClientID != clientID) {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if err != nil {
		s.logger.Error("notification query failed", "task_id", id, "code", "QUERY_FAILED", "error_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	attempts, err := s.store.ListAttempts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notification": n, "attempts": attempts})
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authenticateClient(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !validStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	pageSize, err := strconv.Atoi(r.URL.Query().Get("page_size"))
	if err != nil || pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	if offset > 100000 {
		writeError(w, http.StatusBadRequest, "page is too large")
		return
	}
	items, err := s.store.ListNotifications(r.Context(), clientID, status, pageSize, offset)
	if err != nil {
		s.logger.Error("notification list failed", "code", "LIST_FAILED", "error_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": pageSize})
}

func (s *Server) setFastFail(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticateAdmin(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := decodeLimited(w, r, &request, 8*1024); err != nil || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "enabled and a reason of at most 500 characters are required")
		return
	}
	destination, err := s.store.UpdateFastFail(r.Context(), r.PathValue("id"), request.Enabled, actor, request.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "destination not found")
		return
	}
	if err != nil {
		s.logger.Error("fast-fail update failed", "destination_id", r.PathValue("id"), "code", "FAST_FAIL_UPDATE_FAILED", "error_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.logger.Info("destination fast-fail changed", "destination_id", destination.ID, "actor", actor, "enabled", destination.FastFailEnabled, "reason_recorded", true, "result", "UPDATED")
	writeJSON(w, http.StatusOK, map[string]any{"destination_id": destination.ID, "enabled": destination.FastFailEnabled, "updated_at": destination.UpdatedAt})
}

func (s *Server) resolveNotification(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticateAdmin(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request struct {
		Conclusion string `json:"conclusion"`
	}
	if err := decodeLimited(w, r, &request, 8*1024); err != nil || strings.TrimSpace(request.Conclusion) == "" || len(request.Conclusion) > 1000 {
		writeError(w, http.StatusBadRequest, "conclusion is required and limited to 1000 characters")
		return
	}
	n, err := s.store.Resolve(r.Context(), r.PathValue("id"), actor, request.Conclusion)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if errors.Is(err, store.ErrNotEligible) {
		writeError(w, http.StatusConflict, "notification is not eligible for manual resolution")
		return
	}
	if err != nil {
		s.logger.Error("notification resolution failed", "task_id", r.PathValue("id"), "code", "RESOLUTION_FAILED", "error_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "resolution failed")
		return
	}
	s.logger.Info("notification manually resolved", "task_id", n.ID, "trace_id", n.TraceID, "actor", actor, "result", "RESOLVED")
	writeJSON(w, http.StatusOK, map[string]any{"task_id": n.ID, "status": n.Status, "resolved_by": n.ResolvedBy, "resolution": n.Resolution})
}

func (s *Server) authenticateClient(r *http.Request) (string, bool) {
	token := bearer(r)
	if len(token) != len(s.cfg.ClientToken) || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.ClientToken)) != 1 {
		return "", false
	}
	return s.cfg.ClientID, true
}

func (s *Server) authenticateAdmin(r *http.Request) (string, bool) {
	token := bearer(r)
	if len(token) != len(s.cfg.AdminToken) || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) != 1 {
		return "", false
	}
	return s.cfg.AdminActor, true
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func decodeLimited(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validStatus(s string) bool {
	switch s {
	case domain.StatusPending, domain.StatusProcessing, domain.StatusDelivered, domain.StatusFailed, domain.StatusFastFailed, domain.StatusExpired, domain.StatusUncertain:
		return true
	default:
		return false
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
