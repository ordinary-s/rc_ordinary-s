package api

import (
	"bytes"
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
	"sync/atomic"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/config"
	"github.com/ordinary-s/rc_ordinary-s/internal/domain"
	"github.com/ordinary-s/rc_ordinary-s/internal/signature"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type Server struct {
	cfg           config.Config
	store         *store.Store
	logger        *slog.Logger
	mqReady       func(context.Context) error
	signatureKeys map[string][]byte
	nonceCounter  atomic.Uint64
}

func New(cfg config.Config, s *store.Store, logger *slog.Logger, mqReady func(context.Context) error) *Server {
	return &Server{
		cfg: cfg, store: s, logger: logger, mqReady: mqReady,
		signatureKeys: map[string][]byte{
			cfg.ClientSignatureKeyID: cfg.ClientSignatureKey,
			cfg.AdminSignatureKeyID:  cfg.AdminSignatureKey,
		},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1/notifications/{id}", s.getNotification)
	mux.HandleFunc("GET /api/v1/notifications", s.listNotifications)
	mux.HandleFunc("PATCH /api/v1/admin/destinations/{id}/fast-fail", s.setFastFail)
	mux.HandleFunc("POST /api/v1/admin/notifications/{id}/resolve", s.resolveNotification)
	return s.requestLimits(s.requireSignature(mux))
}

func (s *Server) requestLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.ContentLength > s.cfg.MaxBodyBytes+int64(s.cfg.MaxHeaderBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes+int64(s.cfg.MaxHeaderBytes)+32*1024)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireSignature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				writeError(w, http.StatusRequestEntityTooLarge, "request too large")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		claims, err := signature.VerifyRequest(r, s.signatureKeys, time.Now())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "valid request signature required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.UseSignatureNonce(ctx, claims.KeyID, claims.Nonce, claims.Expires); err != nil {
			if errors.Is(err, store.ErrSignatureReplay) {
				writeError(w, http.StatusUnauthorized, "request signature was already used")
				return
			}
			s.logger.Error("signature nonce persistence failed", "code", "SIGNATURE_NONCE_STORE_FAILED", "error_type", fmt.Sprintf("%T", err))
			writeError(w, http.StatusServiceUnavailable, "request verification is temporarily unavailable")
			return
		}
		if s.nonceCounter.Add(1)%256 == 0 {
			if err := s.store.DeleteExpiredSignatureNonces(ctx); err != nil {
				s.logger.Warn("expired signature nonce cleanup failed", "error_type", fmt.Sprintf("%T", err))
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), signatureKeyIDContextKey{}, claims.KeyID))
		next.ServeHTTP(w, r)
	})
}

type signatureKeyIDContextKey struct{}

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
	keyID, ok := r.Context().Value(signatureKeyIDContextKey{}).(string)
	if !ok || keyID != s.cfg.ClientSignatureKeyID {
		return "", false
	}
	token := bearer(r)
	if len(token) != len(s.cfg.ClientToken) || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.ClientToken)) != 1 {
		return "", false
	}
	return s.cfg.ClientID, true
}

func (s *Server) authenticateAdmin(r *http.Request) (string, bool) {
	keyID, ok := r.Context().Value(signatureKeyIDContextKey{}).(string)
	if !ok || keyID != s.cfg.AdminSignatureKeyID {
		return "", false
	}
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
