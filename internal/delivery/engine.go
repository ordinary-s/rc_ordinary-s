package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/config"
	"github.com/ordinary-s/rc_ordinary-s/internal/domain"
	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type Engine struct {
	cfg       config.Config
	store     *store.Store
	client    *http.Client
	logger    *slog.Logger
	mu        sync.Mutex
	taskLocks map[string]*taskLock
}

type taskLock struct {
	mu   sync.Mutex
	refs int
}

var resultPersistenceBackoffs = [...]time.Duration{50 * time.Millisecond, 100 * time.Millisecond}

type requestBuildError struct {
	code string
	err  error
}

func (e *requestBuildError) Error() string { return e.err.Error() }
func (e *requestBuildError) Unwrap() error { return e.err }

func New(cfg config.Config, s *store.Store, logger *slog.Logger) *Engine {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: cfg.HTTPTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &Engine{
		cfg: cfg, store: s, logger: logger,
		client:    &http.Client{Transport: transport, Timeout: cfg.HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		taskLocks: make(map[string]*taskLock),
	}
}

func (e *Engine) Process(ctx context.Context, event mq.Event, reconsume int) error {
	lock, release := e.lockTask(event.TaskID)
	defer release()
	lock.mu.Lock()
	defer lock.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, e.cfg.DeliveryBudget)
	defer cancel()
	started := time.Now()
	var n domain.Notification
	var err error
	for {
		n, err = e.store.GetNotification(ctx, event.TaskID)
		if err != nil {
			return e.executionError(event, reconsume, started, err, n)
		}
		if domain.IsTerminal(n.Status) {
			e.logResult(n, reconsume, started, n.Status, "TERMINAL_DURING_PROCESSING")
			return nil
		}
		if n.Status == domain.StatusProcessing {
			attempts, err := e.store.ListAttempts(ctx, n.ID)
			if err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			for _, attempt := range attempts {
				if attempt.State == domain.AttemptStarted {
					if err := e.store.MarkInFlightUncertain(ctx, n.ID); err != nil {
						return e.executionError(event, reconsume, started, err, n)
					}
					n.Status = domain.StatusUncertain
					e.logResult(n, reconsume, started, domain.StatusUncertain, "PROCESS_INTERRUPTED_RESULT_UNKNOWN")
					return nil
				}
			}
		}
		if !time.Now().UTC().Before(n.ExpiresAt) {
			if err := e.store.FinishWithoutAttempt(ctx, n.ID, domain.StatusExpired, "TASK_EXPIRED"); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.Status = domain.StatusExpired
			e.logResult(n, reconsume, started, n.Status, "TASK_EXPIRED")
			return nil
		}
		destination, err := e.store.GetDestination(ctx, n.DestinationID)
		if err != nil {
			return e.executionError(event, reconsume, started, err, n)
		}
		if destination.FastFailEnabled {
			if err := e.store.FinishWithoutAttempt(ctx, n.ID, domain.StatusFastFailed, "DESTINATION_FAST_FAIL_ENABLED"); err != nil {
				return e.executionError(event, reconsume, started, err)
			}
			n.Status = domain.StatusFastFailed
			e.logResult(n, reconsume, started, n.Status, "DESTINATION_FAST_FAIL_ENABLED")
			return nil
		}
		if n.NextAttemptAt != nil && time.Now().UTC().Before(*n.NextAttemptAt) {
			timer := time.NewTimer(time.Until(*n.NextAttemptAt))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return e.executionError(event, reconsume, started, ctx.Err(), n)
			case <-timer.C:
				continue
			}
		}
		attemptNo := n.HTTPAttempts + 1
		if attemptNo > 3 {
			if err := e.store.FinishWithoutAttempt(ctx, n.ID, domain.StatusFailed, "HTTP_ATTEMPTS_EXHAUSTED"); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.Status = domain.StatusFailed
			e.logResult(n, reconsume, started, n.Status, "HTTP_ATTEMPTS_EXHAUSTED")
			return nil
		}
		request, cancel, err := makeRequest(ctx, n, destination.URL, e.cfg.HTTPTimeout)
		if err != nil {
			reason := "REQUEST_INVALID"
			var buildErr *requestBuildError
			if errors.As(err, &buildErr) {
				reason = buildErr.code
			}
			e.logger.Warn("outbound request preparation failed", "trace_id", n.TraceID, "task_id", n.ID, "destination_id", n.DestinationID, "code", reason, "error_type", fmt.Sprintf("%T", err))
			if saveErr := e.store.FinishWithoutAttempt(ctx, n.ID, domain.StatusFailed, reason); saveErr != nil {
				return e.executionError(event, reconsume, started, errors.Join(err, saveErr), n)
			}
			n.Status = domain.StatusFailed
			e.logResult(n, reconsume, started, n.Status, reason)
			return nil
		}
		if err := e.store.StartAttempt(ctx, n.ID, attemptNo); err != nil {
			cancel()
			return e.executionError(event, reconsume, started, err, n)
		}
		n.HTTPAttempts = attemptNo
		response, sendErr := e.client.Do(request)
		if sendErr != nil {
			cancel()
			if ctx.Err() != nil {
				return e.executionError(event, reconsume, started, ctx.Err(), n)
			}
			reason := "HTTP_RESULT_UNKNOWN"
			if saveErr := e.completeAttempt(ctx, n.ID, attemptNo, 0, domain.AttemptUnknown, domain.StatusUncertain, reason, nil); saveErr != nil {
				return e.executionError(event, reconsume, started, errors.Join(sendErr, saveErr), n)
			}
			n.HTTPAttempts, n.Status = attemptNo, domain.StatusUncertain
			e.logResult(n, reconsume, started, n.Status, reason)
			return nil
		}
		httpStatus := response.StatusCode
		retryAfter, bodyErr := responseResult(response)
		cancel()
		if bodyErr != nil {
			return e.executionError(event, reconsume, started, bodyErr, n)
		}
		if httpStatus >= 200 && httpStatus < 300 {
			if err := e.completeAttempt(ctx, n.ID, attemptNo, httpStatus, domain.AttemptSucceeded, domain.StatusDelivered, "HTTP_2XX", nil); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.HTTPAttempts, n.HTTPStatus, n.Status = attemptNo, httpStatus, domain.StatusDelivered
			e.logResult(n, reconsume, started, n.Status, "HTTP_2XX")
			return nil
		}
		reason := fmt.Sprintf("HTTP_%d", httpStatus)
		retryable := httpStatus == http.StatusTooManyRequests || httpStatus >= 500
		if !retryable {
			if err := e.completeAttempt(ctx, n.ID, attemptNo, httpStatus, domain.AttemptFailed, domain.StatusFailed, "HTTP_PERMANENT_FAILURE", nil); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.HTTPAttempts, n.HTTPStatus, n.Status = attemptNo, httpStatus, domain.StatusFailed
			e.logResult(n, reconsume, started, n.Status, "HTTP_PERMANENT_FAILURE")
			return nil
		}
		if attemptNo == 3 {
			if err := e.completeAttempt(ctx, n.ID, attemptNo, httpStatus, domain.AttemptFailed, domain.StatusFailed, "HTTP_ATTEMPTS_EXHAUSTED", nil); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.HTTPAttempts, n.HTTPStatus, n.Status = attemptNo, httpStatus, domain.StatusFailed
			e.logResult(n, reconsume, started, n.Status, "HTTP_ATTEMPTS_EXHAUSTED")
			return nil
		}
		delay := e.cfg.Backoffs[attemptNo-1] + time.Duration(rand.Int64N(int64(250*time.Millisecond)+1))
		if retryAfter > delay {
			delay = retryAfter
		}
		remainingWork := remainingAttemptBudget(attemptNo, e.cfg)
		reason = ""
		if !fitsBudget(delay, remainingWork, time.Until(n.ExpiresAt)) {
			reason = "RETRY_DELAY_EXCEEDS_TASK_WINDOW"
		} else if deadline, ok := ctx.Deadline(); ok && !fitsBudget(delay, remainingWork, time.Until(deadline)) {
			reason = "RETRY_DELAY_EXCEEDS_PROCESSING_BUDGET"
		}
		if reason != "" {
			if err := e.completeAttempt(ctx, n.ID, attemptNo, httpStatus, domain.AttemptFailed, domain.StatusFailed, reason, nil); err != nil {
				return e.executionError(event, reconsume, started, err, n)
			}
			n.HTTPStatus, n.Status, n.ResultReason = httpStatus, domain.StatusFailed, reason
			e.logResult(n, reconsume, started, n.Status, reason)
			return nil
		}
		nextAttemptAt := time.Now().UTC().Add(delay).Truncate(time.Microsecond)
		if err := e.completeAttempt(ctx, n.ID, attemptNo, httpStatus, domain.AttemptFailed, domain.StatusProcessing, fmt.Sprintf("HTTP_%d", httpStatus), &nextAttemptAt); err != nil {
			return e.executionError(event, reconsume, started, err, n)
		}
		// The due time is persisted before waiting so MQ redelivery cannot bypass it.
	}
}

func (e *Engine) completeAttempt(ctx context.Context, taskID string, attemptNo, httpStatus int, attemptState, nextTaskStatus, reason string, nextAttemptAt *time.Time) error {
	var lastErr error
	for attempt := 0; attempt <= len(resultPersistenceBackoffs); attempt++ {
		lastErr = e.store.CompleteAttempt(ctx, taskID, attemptNo, httpStatus, attemptState, nextTaskStatus, reason, nextAttemptAt)
		if lastErr == nil {
			return nil
		}
		if attempt == len(resultPersistenceBackoffs) {
			break
		}
		timer := time.NewTimer(resultPersistenceBackoffs[attempt])
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	return lastErr
}

func remainingAttemptBudget(attemptNo int, cfg config.Config) time.Duration {
	remainingCalls := 3 - attemptNo
	budget := time.Duration(remainingCalls)*cfg.HTTPTimeout + 5*time.Second
	for i := attemptNo; i < len(cfg.Backoffs); i++ {
		budget += cfg.Backoffs[i] + 250*time.Millisecond
	}
	return budget
}

func fitsBudget(delay, work, available time.Duration) bool {
	return available > work && delay <= available-work
}

func makeRequest(parent context.Context, n domain.Notification, target string, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, func() {}, &requestBuildError{code: "TARGET_URL_INVALID", err: fmt.Errorf("invalid configured target URL")}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	request, err := http.NewRequestWithContext(ctx, n.Method, u.String(), bytes.NewReader(n.Body))
	if err != nil {
		cancel()
		return nil, func() {}, &requestBuildError{code: "REQUEST_CONSTRUCTION_FAILED", err: fmt.Errorf("create target request: %w", err)}
	}
	request.Header = make(http.Header)
	if len(n.HeadersJSON) != 0 {
		var headers map[string]string
		if err := json.Unmarshal(n.HeadersJSON, &headers); err != nil {
			cancel()
			return nil, func() {}, &requestBuildError{code: "STORED_HEADERS_INVALID", err: fmt.Errorf("decode stored request headers: %w", err)}
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
	}
	request.Header.Set("Idempotency-Key", n.ID)
	request.Header.Set("X-Notification-Task-ID", n.ID)
	request.Header.Set("X-Trace-ID", n.TraceID)
	return request, cancel, nil
}

func responseResult(response *http.Response) (time.Duration, error) {
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 32*1024)); err != nil {
		return 0, fmt.Errorf("read target response: %w", err)
	}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return 0, nil
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		if seconds > int(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), nil
		}
		return time.Duration(seconds) * time.Second, nil
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := time.Until(at)
		if delay > 0 {
			return delay, nil
		}
	}
	return 0, nil
}

func (e *Engine) lockTask(taskID string) (*taskLock, func()) {
	e.mu.Lock()
	lock := e.taskLocks[taskID]
	if lock == nil {
		lock = &taskLock{}
		e.taskLocks[taskID] = lock
	}
	lock.refs++
	e.mu.Unlock()
	return lock, func() {
		e.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(e.taskLocks, taskID)
		}
		e.mu.Unlock()
	}
}

func (e *Engine) executionError(event mq.Event, reconsume int, started time.Time, err error, known ...domain.Notification) error {
	destinationID, attempts := "unknown", "unknown"
	if len(known) > 0 && known[0].ID != "" {
		destinationID = known[0].DestinationID
		attempts = fmt.Sprint(known[0].HTTPAttempts)
	}
	e.logger.Error("notification execution incomplete",
		"trace_id", event.TraceID, "task_id", event.TaskID, "destination_id", destinationID,
		"http_attempts", attempts, "mq_reconsume", reconsume, "result", "EXECUTION_ERROR",
		"code", "EXECUTION_ERROR", "cost_ms", time.Since(started).Milliseconds(), "error_type", fmt.Sprintf("%T", err))
	return err
}

func (e *Engine) logResult(n domain.Notification, reconsume int, started time.Time, result, code string) {
	e.logger.Info("notification processing finished",
		"trace_id", n.TraceID, "task_id", n.ID, "destination_id", n.DestinationID,
		"http_attempts", n.HTTPAttempts, "mq_reconsume", reconsume, "result", result,
		"code", code, "cost_ms", time.Since(started).Milliseconds())
}
