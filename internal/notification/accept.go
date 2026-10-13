package notification

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ordinary-s/rc_ordinary-s/internal/domain"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type Input struct {
	DestinationID string            `json:"destination_id"`
	Method        string            `json:"method"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          string            `json:"body"`
}

var ErrInvalidInput = fmt.Errorf("invalid notification input")

type Service struct {
	store       *store.Store
	allowedHost string
	allowedPort string
	allowedTLS  bool
}

func NewService(s *store.Store, allowedTarget string) (*Service, error) {
	allowed, err := url.Parse(allowedTarget)
	if err != nil || allowed.Hostname() == "" || allowed.User != nil ||
		(allowed.Scheme != "http" && allowed.Scheme != "https") {
		return nil, fmt.Errorf("ALLOWED_TARGET must be an absolute HTTP(S) origin")
	}
	return &Service{
		store:       s,
		allowedHost: strings.ToLower(allowed.Hostname()),
		allowedPort: effectivePort(allowed),
		allowedTLS:  allowed.Scheme == "https",
	}, nil
}

func (s *Service) Accept(ctx context.Context, clientID, idempotencyKey, traceID string, input Input, maxBodyBytes int64, maxHeaderBytes int, ttl time.Duration) (domain.Notification, bool, error) {
	input, body, headerJSON, err := validateInput(input, maxBodyBytes, maxHeaderBytes)
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	destination, err := s.store.GetDestination(ctx, input.DestinationID)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if !validTarget(destination.URL, s.allowedHost, s.allowedPort, s.allowedTLS) {
		return domain.Notification{}, false, fmt.Errorf("destination target is outside the configured allowlist")
	}
	hash, err := fingerprint(input.DestinationID, input.Method, input.Headers, body)
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("fingerprint notification: %w", err)
	}
	id, err := NewID()
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("create task id: %w", err)
	}
	n := domain.Notification{
		ID:             id,
		ClientID:       clientID,
		IdempotencyKey: idempotencyKey,
		Fingerprint:    hash,
		DestinationID:  input.DestinationID,
		Method:         input.Method,
		HeadersJSON:    headerJSON,
		Body:           body,
		TraceID:        traceID,
		ExpiresAt:      time.Now().UTC().Add(ttl),
	}
	return s.store.Accept(ctx, n)
}

func fingerprint(destinationID, method string, headers map[string]string, body []byte) ([]byte, error) {
	type canonical struct {
		DestinationID string            `json:"destination_id"`
		Method        string            `json:"method"`
		Headers       map[string]string `json:"headers"`
		Body          string            `json:"body"`
	}
	data, err := json.Marshal(canonical{DestinationID: destinationID, Method: method, Headers: headers, Body: string(body)})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func effectivePort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func validTarget(targetURL, allowedHost, allowedPort string, allowedTLS bool) bool {
	target, err := url.Parse(targetURL)
	if err != nil || target.User != nil || target.Hostname() == "" || target.Fragment != "" {
		return false
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return false
	}
	return strings.EqualFold(target.Hostname(), allowedHost) && effectivePort(target) == allowedPort &&
		(target.Scheme == "https") == allowedTLS
}

func canonicalHeaders(headers map[string]string, maxHeaderBytes int) (map[string]string, error) {
	result := make(map[string]string, len(headers))
	total := 0
	for name, value := range headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name == "" || strings.ContainsAny(name, " \t\r\n:") {
			return nil, fmt.Errorf("invalid header name")
		}
		lower := strings.ToLower(name)
		switch lower {
		case "host", "content-length", "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "te", "trailer":
			return nil, fmt.Errorf("header %s is not allowed", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("header %s contains a line break", name)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate header name after canonicalization")
		}
		total += len(name) + len(value)
		if total > maxHeaderBytes {
			return nil, fmt.Errorf("request headers exceed %d bytes", maxHeaderBytes)
		}
		result[name] = value
	}
	return result, nil
}

func validateInput(input Input, maxBodyBytes int64, maxHeaderBytes int) (Input, []byte, []byte, error) {
	input.DestinationID = strings.TrimSpace(input.DestinationID)
	input.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if input.DestinationID == "" || len(input.DestinationID) > 64 {
		return input, nil, nil, fmt.Errorf("destination_id is required and must be at most 64 characters")
	}
	switch input.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return input, nil, nil, fmt.Errorf("method is not supported")
	}
	if !utf8.ValidString(input.Body) {
		return input, nil, nil, fmt.Errorf("body must be valid UTF-8")
	}
	body := []byte(input.Body)
	if int64(len(body)) > maxBodyBytes {
		return input, nil, nil, fmt.Errorf("body exceeds %d bytes", maxBodyBytes)
	}
	if len(input.Headers) > 64 {
		return input, nil, nil, fmt.Errorf("at most 64 headers are allowed")
	}
	headers, err := canonicalHeaders(input.Headers, maxHeaderBytes)
	if err != nil {
		return input, nil, nil, err
	}
	input.Headers = headers
	headerJSON, err := json.Marshal(headers)
	if err != nil {
		return input, nil, nil, fmt.Errorf("encode request headers: %w", err)
	}
	return input, body, headerJSON, nil
}
