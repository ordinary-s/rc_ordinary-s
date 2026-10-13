package domain

import (
	"encoding/json"
	"time"
)

const (
	StatusPending    = "PENDING"
	StatusProcessing = "PROCESSING"
	StatusDelivered  = "DELIVERED"
	StatusFailed     = "FAILED"
	StatusFastFailed = "FAST_FAILED"
	StatusExpired    = "EXPIRED"
	StatusUncertain  = "UNCERTAIN"

	AttemptStarted   = "STARTED"
	AttemptSucceeded = "SUCCEEDED"
	AttemptFailed    = "FAILED"
	AttemptUnknown   = "UNKNOWN"
)

type Notification struct {
	ID             string          `json:"task_id"`
	ClientID       string          `json:"client_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Fingerprint    []byte          `json:"-"`
	DestinationID  string          `json:"destination_id"`
	Method         string          `json:"method"`
	HeadersJSON    json.RawMessage `json:"-"`
	Body           []byte          `json:"-"`
	TraceID        string          `json:"trace_id"`
	Status         string          `json:"status"`
	HTTPAttempts   int             `json:"http_attempts"`
	ResultReason   string          `json:"result_reason,omitempty"`
	HTTPStatus     int             `json:"http_status,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	ResolvedBy     string          `json:"resolved_by,omitempty"`
	Resolution     string          `json:"resolution,omitempty"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
}

type Attempt struct {
	Number     int        `json:"attempt_no"`
	State      string     `json:"state"`
	HTTPStatus int        `json:"http_status,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Destination struct {
	ID              string
	URL             string
	FastFailEnabled bool
	UpdatedBy       string
	UpdateReason    string
	UpdatedAt       time.Time
}
