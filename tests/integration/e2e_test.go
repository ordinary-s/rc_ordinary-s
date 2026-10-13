//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	_ "github.com/go-sql-driver/mysql"
)

type taskView struct {
	Notification struct {
		ID           string `json:"task_id"`
		Status       string `json:"status"`
		HTTPAttempts int    `json:"http_attempts"`
		Reason       string `json:"result_reason"`
	} `json:"notification"`
	Attempts []struct {
		State string `json:"state"`
	} `json:"attempts"`
}

type statsView struct {
	Count       int `json:"count"`
	MaxInFlight int `json:"max_in_flight"`
	Items       []struct {
		ContentType string `json:"content_type"`
		Key         string `json:"idempotency_key"`
		TaskID      string `json:"task_id"`
		Body        string `json:"body"`
	} `json:"requests"`
}

type suite struct {
	t      *testing.T
	api    string
	mock   string
	token  string
	admin  string
	db     *sql.DB
	client *http.Client
	report []map[string]any
}

func TestEndToEndWithMySQLAndRocketMQ(t *testing.T) {
	api := env(t, "TEST_API_URL")
	mock := env(t, "TEST_MOCK_URL")
	db, err := sql.Open("mysql", env(t, "TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("MySQL unavailable: %v", err)
	}
	s := &suite{t: t, api: api, mock: mock, token: env(t, "TEST_CLIENT_TOKEN"), admin: env(t, "TEST_ADMIN_TOKEN"), db: db, client: &http.Client{Timeout: 5 * time.Second}, report: make([]map[string]any, 0)}
	defer s.writeReport()

	t.Run("acceptance/idempotency/transaction", func(t *testing.T) {
		s.reset(t)
		id := s.submit(t, "idempotent-a", "application/json", "{\"message\":\"one\",\"body_canary\":\"notification-body-secret-canary-9ba8\"}")
		again := s.submit(t, "idempotent-a", "application/json", "{\"message\":\"one\",\"body_canary\":\"notification-body-secret-canary-9ba8\"}")
		if id != again {
			t.Fatalf("duplicate returned %s, want %s", again, id)
		}
		status, _ := s.create(t, "idempotent-a", "{\"message\":\"different\"}")
		if status != http.StatusConflict {
			t.Fatalf("changed request status=%d, want 409", status)
		}
		task := s.wait(t, id, "DELIVERED")
		stats := s.stats(t)
		var notifications, outboxes int
		if err := db.QueryRow("SELECT COUNT(*) FROM notification WHERE task_id=?", id).Scan(&notifications); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM outbox WHERE task_id=?", id).Scan(&outboxes); err != nil {
			t.Fatal(err)
		}
		if notifications != 1 || outboxes != 1 || task.Notification.HTTPAttempts != 1 || stats.Count != 1 {
			t.Fatalf("unexpected acceptance counts n=%d o=%d attempts=%d calls=%d", notifications, outboxes, task.Notification.HTTPAttempts, stats.Count)
		}
		var state string
		if err := db.QueryRow("SELECT publish_state FROM outbox WHERE task_id=?", id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "PUBLISHED" {
			t.Fatalf("outbox state=%s", state)
		}
		if err := s.republish(id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		if after := s.stats(t).Count; after != 1 {
			t.Fatalf("duplicate success message caused %d HTTP calls", after)
		}
		s.record("acceptance_transaction_and_idempotency", map[string]any{"task_id": id, "notifications": notifications, "outboxes": outboxes, "outbox_state": state, "mock_calls": stats.Count, "after_duplicate": 1})
	})
	t.Run("outbox publish succeeds but local mark fails", s.testOutboxMarkFailure)

	t.Run("case-sensitive keys and concurrent duplicate submit", func(t *testing.T) {
		s.reset(t)
		upper := s.submit(t, "CaseKey", "text/plain", "x")
		lower := s.submit(t, "casekey", "text/plain", "x")
		if upper == lower {
			t.Fatal("binary collation merged case-distinct idempotency keys")
		}
		const workers = 8
		key := "parallel-key"
		payload := map[string]any{"destination_id": "mock-target", "method": "POST", "headers": map[string]string{"Content-Type": "text/plain"}, "body": "parallel"}
		raw, _ := json.Marshal(payload)
		ids := make(chan string, workers)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body := s.post(t, "/api/v1/notifications", raw, key)
				if code != http.StatusAccepted {
					errs <- fmt.Errorf("status %d", code)
					return
				}
				var v struct {
					ID string `json:"task_id"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					errs <- err
					return
				}
				ids <- v.ID
			}()
		}
		wg.Wait()
		close(ids)
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		first := ""
		for id := range ids {
			if first == "" {
				first = id
			}
			if id != first {
				t.Fatalf("concurrent ids differ: %s %s", first, id)
			}
		}
		var n, o int
		if err := db.QueryRow("SELECT COUNT(*) FROM notification WHERE task_id=?", first).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM outbox WHERE task_id=?", first).Scan(&o); err != nil {
			t.Fatal(err)
		}
		if n != 1 || o != 1 {
			t.Fatalf("concurrent creation produced n=%d outboxes=%d", n, o)
		}
		s.wait(t, first, "DELIVERED")
		s.record("case_sensitive_and_parallel_idempotency", map[string]any{"case_distinct": true, "concurrent_task_id": first, "notification_rows": n, "outbox_rows": o})
	})

	t.Run("concurrent tasks keep trace and task log context paired", s.testConcurrentLogContext)

	t.Run("503 then success", func(t *testing.T) {
		s.configure(t, []int{503, 200})
		start := time.Now()
		id := s.submit(t, "retry-success", "text/plain", "retry")
		task := s.wait(t, id, "DELIVERED")
		stats := s.stats(t)
		elapsed := time.Since(start)
		if task.Notification.HTTPAttempts != 2 || len(task.Attempts) != 2 || stats.Count != 2 {
			t.Fatalf("attempts=%d rows=%d calls=%d", task.Notification.HTTPAttempts, len(task.Attempts), stats.Count)
		}
		if elapsed < 35*time.Millisecond {
			t.Fatalf("first backoff not observed: %s", elapsed)
		}
		s.record("http_503_then_success", map[string]any{"task_id": id, "attempts": task.Notification.HTTPAttempts, "mock_calls": stats.Count, "elapsed_ms": elapsed.Milliseconds()})
	})

	t.Run("three 503 and duplicate MQ delivery", func(t *testing.T) {
		s.configure(t, []int{503})
		start := time.Now()
		id := s.submit(t, "retry-exhausted", "text/plain", "must stop")
		task := s.wait(t, id, "FAILED")
		stats := s.stats(t)
		elapsed := time.Since(start)
		if task.Notification.HTTPAttempts != 3 || len(task.Attempts) != 3 || stats.Count != 3 {
			t.Fatalf("attempts=%d rows=%d mock_calls=%d, want 3", task.Notification.HTTPAttempts, len(task.Attempts), stats.Count)
		}
		if task.Notification.Reason != "HTTP_ATTEMPTS_EXHAUSTED" {
			t.Fatalf("reason=%s", task.Notification.Reason)
		}
		if elapsed < 50*time.Millisecond {
			t.Fatalf("two backoffs were not observed: %s", elapsed)
		}
		if err := s.republish(id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		after := s.stats(t)
		if after.Count != 3 {
			t.Fatalf("duplicate terminal message sent HTTP again: %d calls", after.Count)
		}
		s.record("three_503_terminal_duplicate_delivery", map[string]any{"task_id": id, "attempts": task.Notification.HTTPAttempts, "mock_calls": stats.Count, "after_duplicate": after.Count, "elapsed_ms": elapsed.Milliseconds()})
	})

	t.Run("fast fail and manual recovery", func(t *testing.T) {
		s.reset(t)
		s.fastFail(t, true)
		old := s.submit(t, "fast-fail", "text/plain", "must not send")
		task := s.wait(t, old, "FAST_FAILED")
		if task.Notification.HTTPAttempts != 0 || s.stats(t).Count != 0 {
			t.Fatal("fast fail invoked the target")
		}
		s.fastFail(t, false)
		time.Sleep(1500 * time.Millisecond)
		if got := s.task(t, old).Notification.Status; got != "FAST_FAILED" {
			t.Fatalf("old terminal task revived: %s", got)
		}
		if s.stats(t).Count != 0 {
			t.Fatal("old terminal task sent after switch disabled")
		}
		next := s.submit(t, "after-recovery", "text/plain", "new task")
		s.wait(t, next, "DELIVERED")
		if s.stats(t).Count != 1 {
			t.Fatal("new task after recovery was not sent")
		}
		s.record("fast_fail_terminal_manual_recovery", map[string]any{"old_task": old, "new_task": next, "old_calls_after_recovery": 0})
	})

	t.Run("XML and form bodies remain exact", func(t *testing.T) {
		for i, tc := range []struct{ ct, body string }{{"application/xml; charset=utf-8", "<notice><title>你好</title></notice>"}, {"application/x-www-form-urlencoded", "name=hello+world&status=%E5%B7%B2%E5%8F%91%E9%80%81"}} {
			s.reset(t)
			id := s.submit(t, fmt.Sprintf("raw-%d", i), tc.ct, tc.body)
			s.wait(t, id, "DELIVERED")
			stats := s.stats(t)
			if stats.Count != 1 || len(stats.Items) != 1 || stats.Items[0].Body != tc.body || stats.Items[0].ContentType != tc.ct {
				t.Fatalf("raw body mismatch: %#v", stats)
			}
			s.record("raw_body_"+fmt.Sprint(i), map[string]any{"task_id": id, "content_type": tc.ct})
		}
	})

	t.Run("outbox plan and UTC storage", func(t *testing.T) {
		rows, err := db.Query("EXPLAIN SELECT event_id, task_id, trace_id, publish_attempts FROM outbox WHERE publish_state='PENDING' AND next_attempt_at<=UTC_TIMESTAMP(6) ORDER BY next_attempt_at ASC,outbox_id ASC LIMIT 100")
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if !rows.Next() {
			t.Fatal("EXPLAIN returned no row")
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		plan := map[string]any{}
		for i, col := range cols {
			if raw, ok := vals[i].([]byte); ok {
				plan[col] = string(raw)
			} else {
				plan[col] = vals[i]
			}
		}
		rows.Close()
		var tz string
		if err := db.QueryRow("SELECT @@session.time_zone").Scan(&tz); err != nil {
			t.Fatal(err)
		}
		s.record("outbox_explain_utc", map[string]any{"plan": plan, "runner_timezone": tz})
	})

	t.Run("database exception is redelivered by RocketMQ", s.testDatabaseExceptionReconsume)
	t.Run("HTTP success result-write retries before MQ redelivery", s.testResultWriteFailure)
	t.Run("HTTP success result-write exhaustion redelivers without repeating HTTP", s.testResultWriteExhaustion)

	t.Run("native RocketMQ retries reach DLQ", s.testNativeDLQ)
}

func TestBrokerOutageOutboxRecoveryAndExpiry(t *testing.T) {
	db, err := sql.Open("mysql", env(t, "TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &suite{t: t, api: env(t, "TEST_API_URL"), mock: env(t, "TEST_MOCK_URL"), token: env(t, "TEST_CLIENT_TOKEN"), admin: env(t, "TEST_ADMIN_TOKEN"), db: db, client: &http.Client{Timeout: 5 * time.Second}, report: make([]map[string]any, 0)}
	defer s.writeReport()
	control := env(t, "TEST_CONTROL_DIR")
	marker := func(name string) string { return filepath.Join(control, "broker-outage-"+name) }
	if err := os.WriteFile(marker("armed"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, marker("stopped"), 60*time.Second)
	ready, err := s.client.Get(s.api + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz status during broker outage=%d, want 503", ready.StatusCode)
	}
	s.reset(t)
	id := s.submit(t, "broker-down-expired", "text/plain", "must expire before recovery")
	if _, err := db.Exec(`UPDATE notification SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=?`, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	var publishState string
	if err := db.QueryRow(`SELECT publish_state FROM outbox WHERE task_id=?`, id).Scan(&publishState); err != nil {
		t.Fatal(err)
	}
	if publishState != "PENDING" || s.stats(t).Count != 0 {
		t.Fatalf("broker outage did not retain unpublished work: state=%s target_calls=%d", publishState, s.stats(t).Count)
	}
	if err := os.WriteFile(marker("queued"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, marker("resume"), 60*time.Second)
	task := s.wait(t, id, "EXPIRED")
	time.Sleep(1200 * time.Millisecond)
	if s.stats(t).Count != 0 || task.Notification.HTTPAttempts != 0 {
		t.Fatal("expired task started an HTTP request after broker recovery")
	}
	fresh := s.submit(t, "broker-recovered-new-task", "text/plain", "recovered")
	s.wait(t, fresh, "DELIVERED")
	if got := s.stats(t).Count; got != 1 {
		t.Fatalf("new task was not delivered after broker recovery: calls=%d", got)
	}
	var finalState string
	if err := db.QueryRow(`SELECT publish_state FROM outbox WHERE task_id=?`, fresh).Scan(&finalState); err != nil {
		t.Fatal(err)
	}
	if finalState != "PUBLISHED" {
		t.Fatalf("recovered outbox state=%s", finalState)
	}
	s.record("broker_outage_outbox_recovery_and_task_expiry", map[string]any{"expired_task": id, "expired_task_calls": 0, "fresh_task": fresh, "fresh_task_calls": 1, "outbox_state": finalState})
}

func TestSIGTERMInterruptsInFlightDelivery(t *testing.T) {
	db, err := sql.Open("mysql", env(t, "TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &suite{t: t, api: env(t, "TEST_API_URL"), mock: env(t, "TEST_MOCK_URL"), token: env(t, "TEST_CLIENT_TOKEN"), admin: env(t, "TEST_ADMIN_TOKEN"), db: db, client: &http.Client{Timeout: 5 * time.Second}, report: make([]map[string]any, 0)}
	defer s.writeReport()
	control := env(t, "TEST_CONTROL_DIR")
	marker := func(name string) string { return filepath.Join(control, "sigterm-"+name) }
	s.reset(t)
	s.configureDelay(t, []int{200}, 10000)
	id := s.submit(t, "sigterm-in-flight", "text/plain", "interrupted once")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view := s.task(t, id)
		stats := s.stats(t)
		if view.Notification.Status == "PROCESSING" && stats.Count == 1 && len(view.Attempts) == 1 && view.Attempts[0].State == "STARTED" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	view := s.task(t, id)
	if view.Notification.Status != "PROCESSING" || len(view.Attempts) != 1 || view.Attempts[0].State != "STARTED" || s.stats(t).Count != 1 {
		t.Fatalf("HTTP request was not in flight before SIGTERM: status=%s attempts=%#v calls=%d", view.Notification.Status, view.Attempts, s.stats(t).Count)
	}
	if err := os.WriteFile(marker("armed"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, marker("stopped"), 60*time.Second)
	waitFile(t, marker("resume"), 60*time.Second)
	deadline = time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		view = s.task(t, id)
		if view.Notification.Status == "UNCERTAIN" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	stats := s.stats(t)
	if view.Notification.Status != "UNCERTAIN" || view.Notification.HTTPAttempts != 1 || stats.Count != 1 || len(view.Attempts) != 1 || view.Attempts[0].State != "UNKNOWN" {
		t.Fatalf("SIGTERM recovery mismatch: status=%s attempts=%d calls=%d rows=%#v", view.Notification.Status, view.Notification.HTTPAttempts, stats.Count, view.Attempts)
	}
	s.record("sigterm_interrupts_inflight_http_and_redelivery_does_not_repeat", map[string]any{"task_id": id, "status_after_restart": view.Notification.Status, "http_attempts": view.Notification.HTTPAttempts, "mock_calls": stats.Count, "attempt_state": view.Attempts[0].State})
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for host verification marker %s", filepath.Base(path))
}

func (s *suite) testDatabaseExceptionReconsume(t *testing.T) {
	s.reset(t)
	observer := s.observeRetryTopic(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_processing`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_processing BEFORE UPDATE ON notification FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='controlled integration failure'`); err != nil {
		t.Fatal(err)
	}
	drop := func() { _, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_processing`) }
	t.Cleanup(drop)
	id := s.submit(t, "db-exception-redelivery", "text/plain", "only after database recovers")
	message := observer.waitTask(t, id, 30*time.Second)
	drop()
	task := s.wait(t, id, "DELIVERED")
	stats := s.stats(t)
	if message.ReconsumeTimes < 1 || task.Notification.HTTPAttempts != 1 || stats.Count != 1 {
		t.Fatalf("native recovery mismatch: reconsume=%d attempts=%d calls=%d", message.ReconsumeTimes, task.Notification.HTTPAttempts, stats.Count)
	}
	var published string
	if err := s.db.QueryRow(`SELECT publish_state FROM outbox WHERE task_id=?`, id).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != "PUBLISHED" {
		t.Fatalf("outbox state=%s", published)
	}
	s.record("database_exception_native_redelivery", map[string]any{"task_id": id, "mq_reconsume": message.ReconsumeTimes, "http_attempts": task.Notification.HTTPAttempts, "mock_calls": stats.Count, "outbox_state": published})
}

func (s *suite) testResultWriteFailure(t *testing.T) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_result_write_failures`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_result_write_failures (id INT PRIMARY KEY, writes INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_result_write_failures (id, writes) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_delivered BEFORE UPDATE ON notification FOR EACH ROW BEGIN
		DECLARE write_count INT;
		IF NEW.status = 'DELIVERED' THEN
			UPDATE e2e_result_write_failures SET writes = writes + 1 WHERE id = 1;
			SELECT writes INTO write_count FROM e2e_result_write_failures WHERE id = 1;
			IF write_count <= 2 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled transient result write failure'; END IF;
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_result_write_failures`)
	}
	t.Cleanup(drop)
	s.configure(t, []int{200})
	start := time.Now()
	id := s.submit(t, "external-ok-write-fails", "text/plain", "do not send twice")
	task := s.wait(t, id, "DELIVERED")
	stats := s.stats(t)
	if stats.Count != 1 {
		t.Fatalf("HTTP result persistence retried the external request: calls=%d", stats.Count)
	}
	var writes int
	if err := s.db.QueryRow(`SELECT writes FROM e2e_result_write_failures WHERE id = 1`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if writes != 3 || task.Notification.Status != "DELIVERED" || task.Notification.HTTPAttempts != 1 || len(task.Attempts) != 1 || task.Attempts[0].State != "SUCCEEDED" || elapsed < 140*time.Millisecond {
		t.Fatalf("result write retries mismatch: writes=%d attempts=%d rows=%#v", writes, task.Notification.HTTPAttempts, task.Attempts)
	}
	drop()
	s.record("external_success_result_write_retried_without_repeating_http", map[string]any{"task_id": id, "result_write_attempts": writes, "http_attempts": task.Notification.HTTPAttempts, "mock_calls": stats.Count, "attempt_state": task.Attempts[0].State, "result": task.Notification.Status, "elapsed_ms": elapsed.Milliseconds()})
}

func (s *suite) testOutboxMarkFailure(t *testing.T) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_outbox_mark`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_outbox_mark_failures`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_outbox_mark_failures (id INT PRIMARY KEY, failures INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_outbox_mark_failures (id, failures) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_outbox_mark BEFORE UPDATE ON outbox FOR EACH ROW BEGIN
		IF OLD.publish_state = 'PENDING' AND NEW.publish_state = 'PUBLISHED' THEN
			UPDATE e2e_outbox_mark_failures SET failures = failures + 1 WHERE id = 1;
			SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled outbox acknowledgement update failure';
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_outbox_mark`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_outbox_mark_failures`)
	}
	t.Cleanup(drop)
	s.configure(t, []int{200})
	id := s.submit(t, "outbox-mark-fails-after-send", "text/plain", "same event may be published again")
	task := s.wait(t, id, "DELIVERED")
	deadline := time.Now().Add(10 * time.Second)
	var markFailures int
	for time.Now().Before(deadline) {
		if err := s.db.QueryRow(`SELECT failures FROM e2e_outbox_mark_failures WHERE id = 1`).Scan(&markFailures); err != nil {
			t.Fatal(err)
		}
		if markFailures >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if markFailures < 2 {
		t.Fatalf("outbox mark failure did not cause repeated publish attempts: failures=%d", markFailures)
	}
	drop()
	deadline = time.Now().Add(10 * time.Second)
	var publishState string
	for time.Now().Before(deadline) {
		if err := s.db.QueryRow(`SELECT publish_state FROM outbox WHERE task_id = ?`, id).Scan(&publishState); err != nil {
			t.Fatal(err)
		}
		if publishState == "PUBLISHED" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if publishState != "PUBLISHED" {
		t.Fatalf("outbox did not mark published after database recovery: %s", publishState)
	}
	time.Sleep(1500 * time.Millisecond)
	stats := s.stats(t)
	if stats.Count != 1 || task.Notification.HTTPAttempts != 1 {
		t.Fatalf("duplicate published events repeated HTTP: calls=%d attempts=%d", stats.Count, task.Notification.HTTPAttempts)
	}
	s.record("outbox_send_mark_failure_duplicate_safe", map[string]any{"task_id": id, "local_mark_failures": markFailures, "outbox_state_after_recovery": publishState, "mock_calls_after_duplicate_window": stats.Count, "http_attempts": task.Notification.HTTPAttempts})
}

func (s *suite) testResultWriteExhaustion(t *testing.T) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered_exhaustion`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_result_write_exhaustion`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_result_write_exhaustion (id INT PRIMARY KEY, writes INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_result_write_exhaustion (id, writes) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_delivered_exhaustion BEFORE UPDATE ON notification FOR EACH ROW BEGIN
		DECLARE write_count INT;
		IF NEW.status = 'DELIVERED' THEN
			UPDATE e2e_result_write_exhaustion SET writes = writes + 1 WHERE id = 1;
			SELECT writes INTO write_count FROM e2e_result_write_exhaustion WHERE id = 1;
			IF write_count <= 3 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled permanent result write failure'; END IF;
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered_exhaustion`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_result_write_exhaustion`)
	}
	t.Cleanup(drop)
	observer := s.observeRetryTopic(t)
	s.configure(t, []int{200})
	id := s.submit(t, "external-ok-result-write-exhausted", "text/plain", "do not repeat successful HTTP")
	retryMessage := observer.waitTask(t, id, 30*time.Second)
	var writes int
	if err := s.db.QueryRow(`SELECT writes FROM e2e_result_write_exhaustion WHERE id = 1`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 3 {
		t.Fatalf("result persistence attempts=%d, want bounded total of 3", writes)
	}
	drop()
	task := s.wait(t, id, "UNCERTAIN")
	if retryMessage.ReconsumeTimes < 1 || len(task.Attempts) != 1 || task.Attempts[0].State != "UNKNOWN" {
		t.Fatalf("recovery did not preserve unknown external result: reconsume=%d attempts=%#v", retryMessage.ReconsumeTimes, task.Attempts)
	}
	time.Sleep(1500 * time.Millisecond)
	stats := s.stats(t)
	if stats.Count != 1 || task.Notification.HTTPAttempts != 1 {
		t.Fatalf("MQ redelivery repeated an externally successful request: calls=%d attempts=%d", stats.Count, task.Notification.HTTPAttempts)
	}
	s.record("external_success_result_write_exhaustion_no_http_repeat", map[string]any{"task_id": id, "result_write_attempts": writes, "mq_reconsume": retryMessage.ReconsumeTimes, "http_attempts": task.Notification.HTTPAttempts, "mock_calls_after_duplicate_window": stats.Count, "attempt_state": task.Attempts[0].State, "result": task.Notification.Status})
}

func (s *suite) testConcurrentLogContext(t *testing.T) {
	s.reset(t)
	s.configureDelay(t, []int{200}, 500)
	const taskCount = 8
	type logContext struct {
		TaskID  string `json:"task_id"`
		TraceID string `json:"trace_id"`
	}
	contexts := make([]logContext, 0, taskCount)
	for i := 0; i < taskCount; i++ {
		id, trace := s.submitWithTrace(t, fmt.Sprintf("log-context-%d", i), "text/plain", fmt.Sprintf("private-body-%d", i))
		contexts = append(contexts, logContext{TaskID: id, TraceID: trace})
	}
	for _, item := range contexts {
		task := s.wait(t, item.TaskID, "DELIVERED")
		if task.Notification.HTTPAttempts != 1 {
			t.Fatalf("task %s attempts=%d", item.TaskID, task.Notification.HTTPAttempts)
		}
	}
	stats := s.stats(t)
	if stats.Count != taskCount || stats.MaxInFlight < 2 || stats.MaxInFlight > 4 {
		t.Fatalf("bounded parallel mock calls mismatch: calls=%d max_in_flight=%d", stats.Count, stats.MaxInFlight)
	}
	control := os.Getenv("TEST_CONTROL_DIR")
	if control == "" {
		t.Fatal("TEST_CONTROL_DIR is required for the host log-context assertion")
	}
	var rows strings.Builder
	for _, item := range contexts {
		fmt.Fprintf(&rows, "%s\t%s\n", item.TaskID, item.TraceID)
	}
	if err := os.WriteFile(filepath.Join(control, "log-context.tsv"), []byte(rows.String()), 0600); err != nil {
		t.Fatal(err)
	}
	s.record("concurrent_tasks_keep_trace_and_task_context_paired", map[string]any{"tasks": contexts, "mock_calls": stats.Count, "max_in_flight": stats.MaxInFlight, "configured_consumer_concurrency": 4})
}

type retryObserver struct {
	messages chan *primitive.MessageExt
	stopFn   func() error
}

func (o *retryObserver) stop() error { return o.stopFn() }

func (s *suite) observeRetryTopic(t *testing.T) *retryObserver {
	t.Helper()
	ns, err := resolveNameServer(env(t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		t.Fatal(err)
	}
	resolver := primitive.NewPassthroughResolver([]string{ns})
	client, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName("notification_retry_audit"),
		consumer.WithNsResolver(resolver),
		consumer.WithConsumeMessageBatchMaxSize(1), consumer.WithPullBatchSize(1), consumer.WithConsumeGoroutineNums(1),
		consumer.WithMaxReconsumeTimes(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	observer := &retryObserver{messages: make(chan *primitive.MessageExt, 16), stopFn: client.Shutdown}
	if err := client.Subscribe("%RETRY%notification_delivery_workers", consumer.MessageSelector{}, func(_ context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
		for _, message := range messages {
			select {
			case observer.messages <- message:
			default:
			}
		}
		return consumer.ConsumeSuccess, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.stop() })
	return observer
}

func (o *retryObserver) waitTask(t *testing.T, taskID string, timeout time.Duration) *primitive.MessageExt {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case message := <-o.messages:
			var event struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal(message.Body, &event) == nil && event.TaskID == taskID {
				return message
			}
		case <-timer.C:
			t.Fatalf("task %s did not appear on RocketMQ retry topic within %s", taskID, timeout)
		}
	}
}

func (s *suite) testNativeDLQ(t *testing.T) {
	ns, err := resolveNameServer(env(t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		t.Fatal(err)
	}
	resolver := primitive.NewPassthroughResolver([]string{ns})
	dlq := make(chan *primitive.MessageExt, 1)
	c, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName("notification_dlq_audit"),
		consumer.WithNsResolver(resolver),
		consumer.WithConsumeMessageBatchMaxSize(1),
		consumer.WithPullBatchSize(1),
		consumer.WithConsumeGoroutineNums(1),
		consumer.WithMaxReconsumeTimes(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Shutdown(); err != nil {
			t.Errorf("shutdown DLQ observer: %v", err)
		}
	})
	dlqTopic := "%DLQ%notification_delivery_workers"
	if err := c.Subscribe(dlqTopic, consumer.MessageSelector{}, func(_ context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
		if len(messages) != 1 {
			return consumer.ConsumeRetryLater, fmt.Errorf("DLQ callback received %d messages", len(messages))
		}
		select {
		case dlq <- messages[0]:
		default:
		}
		return consumer.ConsumeSuccess, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	p, err := rocketmq.NewProducer(producer.WithGroupName("notification_integration_poison"), producer.WithNsResolver(resolver), producer.WithRetry(0), producer.WithSendMsgTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	event := map[string]string{"event_id": "integration-native-dlq", "task_id": "missing-task-for-dlq", "trace_id": "integration-dlq"}
	body, _ := json.Marshal(event)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result, err := p.SendSync(ctx, primitive.NewMessage(env(t, "ROCKETMQ_TOPIC"), body))
	cancel()
	shutdownErr := p.Shutdown()
	if err != nil || result == nil || result.Status != primitive.SendOK {
		t.Fatalf("poison message send was not confirmed: result=%v error=%v shutdown=%v", result, err, shutdownErr)
	}
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	select {
	case message := <-dlq:
		if message == nil || message.Topic != dlqTopic {
			t.Fatalf("unexpected dead letter: %#v", message)
		}
		s.record("rocketmq_native_retries_reach_dlq", map[string]any{"topic": message.Topic, "reconsume_times": message.ReconsumeTimes, "msg_id": message.MsgId})
	case <-timer.C:
		t.Fatal("native RocketMQ retry did not reach DLQ within three minutes")
	}
}

func (s *suite) submit(t *testing.T, key, contentType, body string) string {
	id, _ := s.submitWithTrace(t, key, contentType, body)
	return id
}

func (s *suite) submitWithTrace(t *testing.T, key, contentType, body string) (string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"destination_id": "mock-target", "method": "POST", "headers": map[string]string{"Content-Type": contentType}, "body": body})
	code, data := s.post(t, "/api/v1/notifications", payload, key)
	if code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", code, data)
	}
	var result struct {
		ID      string `json:"task_id"`
		TraceID string `json:"trace_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" {
		t.Fatal("missing task_id")
	}
	if result.TraceID == "" {
		t.Fatal("missing trace_id")
	}
	return result.ID, result.TraceID
}

func (s *suite) create(t *testing.T, key, body string) (int, []byte) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"destination_id": "mock-target", "method": "POST", "headers": map[string]string{"Content-Type": "application/json"}, "body": body})
	return s.post(t, "/api/v1/notifications", payload, key)
}

func (s *suite) post(t *testing.T, path string, body []byte, key string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.api+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func (s *suite) task(t *testing.T, id string) taskView {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.api+"/api/v1/notifications/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v taskView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get task status=%d", resp.StatusCode)
	}
	return v
}

func (s *suite) wait(t *testing.T, id, want string) taskView {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		v := s.task(t, id)
		if v.Notification.Status == want {
			return v
		}
		switch v.Notification.Status {
		case "DELIVERED", "FAILED", "FAST_FAILED", "EXPIRED", "UNCERTAIN":
			t.Fatalf("task reached %s, want %s (%s)", v.Notification.Status, want, v.Notification.Reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", want)
	return taskView{}
}

func (s *suite) stats(t *testing.T) statsView {
	t.Helper()
	resp, err := s.client.Get(s.mock + "/control/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v statsView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (s *suite) reset(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.mock+"/control/reset", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status=%d", resp.StatusCode)
	}
}

func (s *suite) configure(t *testing.T, statuses []int) {
	s.configureDelay(t, statuses, 0)
}

func (s *suite) configureDelay(t *testing.T, statuses []int, delayMS int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"statuses": statuses, "delay_ms": delayMS, "response_body": "controlled"})
	req, _ := http.NewRequest(http.MethodPost, s.mock+"/control/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("configure status=%d", resp.StatusCode)
	}
}

func (s *suite) fastFail(t *testing.T, enabled bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"enabled": enabled, "reason": "integration test"})
	req, _ := http.NewRequest(http.MethodPatch, s.api+"/api/v1/admin/destinations/mock-target/fast-fail", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.admin)
	req.Header.Set("X-Admin-Actor", "integration-test")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fast-fail status=%d", resp.StatusCode)
	}
}

func (s *suite) republish(taskID string) error {
	var event struct {
		EventID string `json:"event_id"`
		TaskID  string `json:"task_id"`
		TraceID string `json:"trace_id"`
	}
	if err := s.db.QueryRow("SELECT event_id,task_id,trace_id FROM outbox WHERE task_id=?", taskID).Scan(&event.EventID, &event.TaskID, &event.TraceID); err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ns, err := resolveNameServer(env(s.t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		return err
	}
	resolver := primitive.NewPassthroughResolver([]string{ns})
	p, err := rocketmq.NewProducer(producer.WithGroupName("notification_integration_duplicate"), producer.WithNsResolver(resolver), producer.WithRetry(0), producer.WithSendMsgTimeout(5*time.Second))
	if err != nil {
		return err
	}
	if err = p.Start(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.SendSync(ctx, primitive.NewMessage(env(s.t, "ROCKETMQ_TOPIC"), body))
	if err == nil && (result == nil || result.Status != primitive.SendOK) {
		err = fmt.Errorf("duplicate not confirmed: %v", result)
	}
	return errors.Join(err, p.Shutdown())
}

func resolveNameServer(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), port), nil
		}
	}
	if len(ips) > 0 {
		return net.JoinHostPort(ips[0].String(), port), nil
	}
	return "", fmt.Errorf("no IP address for %q", host)
}

func (s *suite) record(name string, details map[string]any) {
	s.report = append(s.report, map[string]any{"name": name, "passed": true, "details": details})
}
func (s *suite) writeReport() {
	path := os.Getenv("TEST_REPORT")
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	raw, err := json.MarshalIndent(map[string]any{"scenarios": s.report}, "", "  ")
	if err == nil {
		_ = os.WriteFile(path, raw, 0644)
	}
}
func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is required", key)
	}
	return v
}
