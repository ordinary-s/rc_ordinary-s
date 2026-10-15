//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
	"github.com/ordinary-s/rc_ordinary-s/internal/notification"
	"github.com/ordinary-s/rc_ordinary-s/internal/signature"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type taskView struct {
	Notification struct {
		ID           string `json:"task_id"`
		TraceID      string `json:"trace_id"`
		Destination  string `json:"destination_id"`
		Status       string `json:"status"`
		HTTPAttempts int    `json:"http_attempts"`
		Reason       string `json:"result_reason"`
		Resolution   string `json:"resolution"`
	} `json:"notification"`
	Attempts []struct {
		State string `json:"state"`
	} `json:"attempts"`
}

type statsView struct {
	Count       int `json:"count"`
	MaxInFlight int `json:"max_in_flight"`
	Requests    []struct {
		ContentType string `json:"content_type"`
		Key         string `json:"idempotency_key"`
		TaskID      string `json:"task_id"`
		TraceID     string `json:"trace_id"`
		Body        string `json:"body"`
	} `json:"requests"`
}

type suite struct {
	t           *testing.T
	api         string
	mock        string
	clientID    string
	token       string
	admin       string
	clientKeyID string
	clientKey   []byte
	adminKeyID  string
	adminKey    []byte
	db          *sql.DB
	client      *http.Client
	producer    rocketmq.Producer
	report      []map[string]any
	contexts    [][2]string
}

type retryObserver struct {
	messages chan *primitive.MessageExt
	consumer rocketmq.PushConsumer
}

type dlqObserver struct {
	messages chan *primitive.MessageExt
	consumer rocketmq.PushConsumer
}

func TestEndToEndWithMySQLAndRocketMQ(t *testing.T) {
	apiURL := env(t, "TEST_API_URL")
	mockURL := env(t, "TEST_MOCK_URL")
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

	s := newSuite(t, apiURL, mockURL, db)
	defer s.close()
	defer s.writeReport()
	if err := s.startProducer(); err != nil {
		t.Fatal(err)
	}

	retry := s.observeRetries(t)
	dlq := s.observeDLQ(t)
	t.Run("TLS HMAC and removed HTTP submit route", func(t *testing.T) { s.testAPISecurityAndRemovedSubmit(t) })
	t.Run("MQ event dedupe, payload conflict, raw body and demo producer", func(t *testing.T) { s.testMQIngressAndDemoProducer(t, dlq) })
	t.Run("concurrent duplicate MQ delivery uses one task", func(t *testing.T) { s.testConcurrentDuplicateIngress(t) })
	t.Run("transient HTTP failure succeeds on the second business attempt", func(t *testing.T) { s.testSecondAttemptSuccess(t) })
	t.Run("three HTTP attempts then terminal ACK", func(t *testing.T) { s.testThreeAttemptsAndTerminalDuplicate(t) })
	t.Run("notification commit before HTTP and native recovery", func(t *testing.T) { s.testIngestCommitBeforeAttempt(t, retry) })
	t.Run("notification insert failure does not call HTTP", func(t *testing.T) { s.testNotificationInsertFailure(t, retry) })
	t.Run("fast fail, expiry and manual resolution", func(t *testing.T) { s.testTerminalPoliciesAndAdmin(t) })
	t.Run("result write failure does not repeat HTTP", func(t *testing.T) { s.testResultWriteFailure(t, retry) })
	t.Run("protocol errors and execution error reach native DLQ", func(t *testing.T) { s.testNativeDLQPaths(t, dlq) })
	t.Run("task identity is scoped by trusted client", func(t *testing.T) { s.testClientScopedEventKeys(t) })
	s.saveContexts()
}

func TestSIGTERMInterruptsInFlightDelivery(t *testing.T) {
	db, err := sql.Open("mysql", env(t, "TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newSuite(t, env(t, "TEST_API_URL"), env(t, "TEST_MOCK_URL"), db)
	defer s.close()
	defer s.writeReport()
	if err := s.startProducer(); err != nil {
		t.Fatal(err)
	}
	s.reset(t)
	s.configure(t, []int{200}, 8000)
	event := s.newEvent("e2e-sigterm-"+shortID(), "in-flight must remain uncertain after stop")
	s.sendEvent(t, event)
	taskID := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s.stats(t).Count > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.stats(t).Count == 0 {
		t.Fatal("mock target did not observe the in-flight delivery")
	}
	control := env(t, "TEST_CONTROL_DIR")
	if err := os.WriteFile(filepath.Join(control, "sigterm-armed"), []byte("armed"), 0600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(control, "sigterm-stopped"), 120*time.Second)
	waitForFile(t, filepath.Join(control, "sigterm-resume"), 180*time.Second)
	task := s.wait(t, taskID, "UNCERTAIN", 60*time.Second)
	if task.Notification.HTTPAttempts != 1 || s.stats(t).Count != 1 {
		t.Fatalf("SIGTERM recovery repeated HTTP: attempts=%d mock_calls=%d", task.Notification.HTTPAttempts, s.stats(t).Count)
	}
	s.record("sigterm_inflight_is_recovered_as_uncertain", map[string]any{"task_id": taskID, "http_attempts": 1, "mock_calls": 1, "status": task.Notification.Status})
	s.saveContexts()
}

func newSuite(t *testing.T, apiURL, mockURL string, db *sql.DB) *suite {
	t.Helper()
	ca, err := os.ReadFile(env(t, "TEST_TLS_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("test CA file contained no certificates")
	}
	clientKey, err := hex.DecodeString(env(t, "TEST_CLIENT_SIGNATURE_SECRET_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	adminKey, err := hex.DecodeString(env(t, "TEST_ADMIN_SIGNATURE_SECRET_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}
	return &suite{
		t: t, api: apiURL, mock: mockURL, clientID: env(t, "TEST_CLIENT_ID"),
		token: env(t, "TEST_CLIENT_TOKEN"), admin: env(t, "TEST_ADMIN_TOKEN"),
		clientKeyID: env(t, "TEST_CLIENT_SIGNATURE_KEY_ID"), clientKey: clientKey,
		adminKeyID: env(t, "TEST_ADMIN_SIGNATURE_KEY_ID"), adminKey: adminKey,
		db: db, client: &http.Client{Timeout: 6 * time.Second, Transport: transport},
		report: make([]map[string]any, 0), contexts: make([][2]string, 0),
	}
}

func (s *suite) startProducer() error {
	nameserver, err := resolveNameServer(env(s.t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		return err
	}
	client, err := rocketmq.NewProducer(
		producer.WithGroupName("notification_integration_upstream"),
		producer.WithNsResolver(primitive.NewPassthroughResolver([]string{nameserver})),
		producer.WithRetry(0), producer.WithSendMsgTimeout(5*time.Second),
	)
	if err != nil {
		return err
	}
	if err := client.Start(); err != nil {
		return err
	}
	s.producer = client
	return nil
}

func (s *suite) close() {
	if s.producer != nil {
		_ = s.producer.Shutdown()
	}
}

func (s *suite) testAPISecurityAndRemovedSubmit(t *testing.T) {
	s.reset(t)
	resp, err := s.client.Get(s.api + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		resp.Body.Close()
		t.Fatalf("TLS negotiation=%v, want TLS 1.3", resp.TLS)
	}
	resp.Body.Close()
	tls12 := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, RootCAs: s.client.Transport.(*http.Transport).TLSClientConfig.RootCAs}}}
	if resp, err := tls12.Get(s.api + "/healthz"); err == nil {
		resp.Body.Close()
		t.Fatal("TLS 1.2-only client unexpectedly connected")
	}

	unsigned, _ := http.NewRequest(http.MethodGet, s.api+"/api/v1/notifications", nil)
	unsigned.Header.Set("Authorization", "Bearer "+s.token)
	resp, err = s.client.Do(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned API request status=%d, want 401", resp.StatusCode)
	}

	oldSubmit := s.signedRequest(t, http.MethodPost, "/api/v1/notifications", []byte(`{"body":"must-not-be-accepted"}`), false)
	resp, err = s.client.Do(oldSubmit)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("removed submit endpoint status=%d, want 404 or 405", resp.StatusCode)
	}

	clientOnAdmin := s.signedRequest(t, http.MethodPatch, "/api/v1/admin/destinations/mock-target/fast-fail", []byte(`{"enabled":false,"reason":"role test"}`), false)
	resp, err = s.client.Do(clientOnAdmin)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("client credentials reached admin route: status=%d", resp.StatusCode)
	}
	s.record("tls13_hmac_role_separation_and_no_http_submit", map[string]any{"tls13": true, "tls12_rejected": true, "unsigned_api_rejected": true, "post_notifications_status": resp.StatusCode})
}

func (s *suite) testMQIngressAndDemoProducer(t *testing.T, dlq *dlqObserver) {
	s.reset(t)
	toolDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	eventID := "demo-fixed-" + shortID()
	event := s.newEvent(eventID, "<notice>XML body stays raw</notice>")
	event.Headers = map[string]string{"Content-Type": "application/xml; charset=utf-8", "X-Format": "raw"}
	command := exec.Command("go", "run", "../../cmd/demo-producer", "-event-id", event.EventID, "-trace-id", event.TraceID,
		"-destination", event.DestinationID, "-method", event.Method, "-content-type", event.Headers["Content-Type"],
		"-body", event.Body, "-expires-at", event.ExpiresAt, "-count", "2")
	command.Dir = toolDir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("demo producer failed: %v (%s)", err, safeOutput(output))
	}
	if !strings.Contains(string(output), "sends=2") {
		t.Fatalf("demo producer did not report two confirmed sends: %s", safeOutput(output))
	}
	taskID := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	task := s.wait(t, taskID, "DELIVERED", 20*time.Second)
	stats := s.stats(t)
	if task.Notification.HTTPAttempts != 1 || stats.Count != 1 || stats.Requests[0].Body != event.Body || stats.Requests[0].ContentType != event.Headers["Content-Type"] {
		t.Fatalf("duplicate MQ event or XML body mismatch: attempts=%d calls=%d content_type=%q", task.Notification.HTTPAttempts, stats.Count, stats.Requests[0].ContentType)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification WHERE client_id=? AND source_event_id=?`, s.clientID, event.EventID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("notification rows=%d, query error=%v", rows, err)
	}
	s.contexts = append(s.contexts, [2]string{taskID, event.TraceID})
	s.record("versioned_mq_ingress_and_demo_producer_duplicate", map[string]any{"event_id": event.EventID, "task_id": taskID, "duplicate_sends": 2, "notification_rows": rows, "http_calls": stats.Count, "body_format": "XML raw text"})

	// Same event key with changed execution content is rejected without changing the stored task.
	conflict := event
	conflict.Body = "conflicting content must never be sent"
	conflictRaw, _ := json.Marshal(conflict)
	s.sendRaw(t, conflictRaw)
	if message := dlq.waitBody(t, conflictRaw, 2*time.Minute); message.ReconsumeTimes < 2 {
		t.Fatalf("event conflict reached DLQ after only %d retries", message.ReconsumeTimes)
	}
	var storedBody string
	if err := s.db.QueryRow(`SELECT request_body FROM notification WHERE task_id=?`, taskID).Scan(&storedBody); err != nil || storedBody != event.Body {
		t.Fatalf("conflicting content changed stored task: error=%v", err)
	}
	if s.stats(t).Count != 1 {
		t.Fatal("conflict content was sent downstream")
	}
	s.record("same_event_id_with_changed_content_goes_to_dlq_without_overwrite", map[string]any{"task_id": taskID, "http_calls": 1})

	// Form text is also passed as the original UTF-8 body and header value.
	s.reset(t)
	form := s.newEvent("demo-form-"+shortID(), "name=codex&note=%E4%BD%A0%E5%A5%BD")
	form.Headers = map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"}
	command = exec.Command("go", "run", "../../cmd/demo-producer", "-event-id", form.EventID, "-trace-id", form.TraceID,
		"-destination", form.DestinationID, "-method", form.Method, "-content-type", form.Headers["Content-Type"],
		"-body", form.Body, "-expires-at", form.ExpiresAt)
	command.Dir = toolDir
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("form demo producer failed: %v (%s)", err, safeOutput(output))
	}
	formTaskID := s.waitTaskIDByEvent(t, form.EventID, 20*time.Second)
	s.wait(t, formTaskID, "DELIVERED", 20*time.Second)
	formStats := s.stats(t)
	if formStats.Count != 1 || formStats.Requests[0].Body != form.Body || formStats.Requests[0].ContentType != form.Headers["Content-Type"] {
		t.Fatalf("form text was not passed through: calls=%d body=%q content_type=%q", formStats.Count, formStats.Requests[0].Body, formStats.Requests[0].ContentType)
	}
	s.contexts = append(s.contexts, [2]string{formTaskID, form.TraceID})
	s.record("form_body_and_content_type_are_preserved", map[string]any{"task_id": formTaskID, "http_calls": formStats.Count, "body_format": "application/x-www-form-urlencoded"})
}

func (s *suite) testConcurrentDuplicateIngress(t *testing.T) {
	s.reset(t)
	s.configure(t, []int{200}, 700)
	event := s.newEvent("e2e-concurrent-"+shortID(), "one task despite concurrent duplicate events")
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	const copies = 8
	topic := env(t, "ROCKETMQ_TOPIC")
	var group sync.WaitGroup
	errs := make(chan error, copies)
	for i := 0; i < copies; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			message := primitive.NewMessage(topic, body)
			message.WithKeys([]string{event.EventID})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, sendErr := s.producer.SendSync(ctx, message)
			if sendErr != nil {
				errs <- sendErr
				return
			}
			if result == nil || result.Status != primitive.SendOK {
				errs <- fmt.Errorf("upstream duplicate send was not confirmed: result=%v", result)
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	id := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	task := s.wait(t, id, "DELIVERED", 20*time.Second)
	time.Sleep(2 * time.Second)
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification WHERE client_id=? AND source_event_id=?`, s.clientID, event.EventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || task.Notification.HTTPAttempts != 1 || s.stats(t).Count != 1 {
		t.Fatalf("concurrent duplicate ingress created repeated work: rows=%d attempts=%d calls=%d", count, task.Notification.HTTPAttempts, s.stats(t).Count)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("concurrent_duplicate_mq_events_share_one_task_and_http_call", map[string]any{"task_id": id, "messages": copies, "notification_rows": count, "http_calls": s.stats(t).Count})
}

func (s *suite) testSecondAttemptSuccess(t *testing.T) {
	s.reset(t)
	s.configure(t, []int{503, 200}, 0)
	event := s.newEvent("e2e-second-success-"+shortID(), "first request fails, second succeeds")
	s.sendEvent(t, event)
	id := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	task := s.wait(t, id, "DELIVERED", 20*time.Second)
	if task.Notification.HTTPAttempts != 2 || len(task.Attempts) != 2 || s.stats(t).Count != 2 {
		t.Fatalf("second-attempt success mismatch: attempts=%d rows=%d calls=%d", task.Notification.HTTPAttempts, len(task.Attempts), s.stats(t).Count)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("first_http_503_then_second_attempt_success", map[string]any{"task_id": id, "http_business_attempts": 2, "mock_calls": s.stats(t).Count, "status": task.Notification.Status})
}

func (s *suite) testThreeAttemptsAndTerminalDuplicate(t *testing.T) {
	s.reset(t)
	s.configure(t, []int{503, 503, 503}, 0)
	event := s.newEvent("e2e-503-"+shortID(), "retry within the three-call budget")
	s.sendEvent(t, event)
	id := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	task := s.wait(t, id, "FAILED", 40*time.Second)
	if task.Notification.HTTPAttempts != 3 || len(task.Attempts) != 3 || s.stats(t).Count != 3 {
		t.Fatalf("503 attempts/calls mismatch: attempts=%d rows=%d calls=%d", task.Notification.HTTPAttempts, len(task.Attempts), s.stats(t).Count)
	}
	s.sendEvent(t, event)
	time.Sleep(1500 * time.Millisecond)
	if after := s.stats(t).Count; after != 3 {
		t.Fatalf("terminal duplicate increased HTTP calls to %d", after)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("three_503_calls_are_terminal_and_duplicate_safe", map[string]any{"task_id": id, "http_business_attempts": task.Notification.HTTPAttempts, "mock_calls": s.stats(t).Count, "terminal": task.Notification.Status})
}

func (s *suite) testIngestCommitBeforeAttempt(t *testing.T, retry *retryObserver) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_first_attempt`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_attempt_gate`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_attempt_gate (id INT PRIMARY KEY, failures INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_attempt_gate (id, failures) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	event := s.newEvent("e2e-before-http-"+shortID(), "must be sent only after its attempt is persisted")
	id := s.ingestForFaultTest(t, event, s.clientID)
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_first_attempt BEFORE INSERT ON delivery_attempt FOR EACH ROW BEGIN
		IF NEW.task_id = '` + id + `' THEN
			UPDATE e2e_attempt_gate SET failures = failures + 1 WHERE id = 1;
			IF (SELECT failures FROM e2e_attempt_gate WHERE id = 1) = 1 THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled pre-HTTP attempt persistence failure';
			END IF;
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_first_attempt`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_attempt_gate`)
	}()
	s.sendEvent(t, event)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var failures int
		if err := s.db.QueryRow(`SELECT failures FROM e2e_attempt_gate WHERE id=1`).Scan(&failures); err != nil {
			t.Fatal(err)
		}
		if failures > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	var failures int
	if err := s.db.QueryRow(`SELECT failures FROM e2e_attempt_gate WHERE id=1`).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("pre-HTTP failure trigger calls=%d, error=%v", failures, err)
	}
	if s.stats(t).Count != 0 {
		t.Fatal("HTTP was called before attempt persistence succeeded")
	}
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_first_attempt`); err != nil {
		t.Fatal(err)
	}
	retryMsg := retry.wait(t, event.EventID, 30*time.Second)
	if got := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second); got != id {
		t.Fatalf("redelivery changed task ID from %s to %s", id, got)
	}
	task := s.wait(t, id, "DELIVERED", 45*time.Second)
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification WHERE client_id=? AND source_event_id=?`, s.clientID, event.EventID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || task.Notification.HTTPAttempts != 1 || s.stats(t).Count != 1 || retryMsg.ReconsumeTimes < 1 {
		t.Fatalf("pre-HTTP recovery mismatch: rows=%d attempts=%d calls=%d reconsume=%d", rows, task.Notification.HTTPAttempts, s.stats(t).Count, retryMsg.ReconsumeTimes)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("persisted_task_before_http_recovers_on_native_redelivery", map[string]any{"task_id": id, "notification_rows": rows, "http_calls": 1, "mq_reconsume": retryMsg.ReconsumeTimes, "final_status": task.Notification.Status})
}

func (s *suite) testNotificationInsertFailure(t *testing.T, retry *retryObserver) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_first_notification_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_ingest_gate`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_ingest_gate (id INT PRIMARY KEY, failures INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_ingest_gate (id, failures) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_first_notification_insert BEFORE INSERT ON notification FOR EACH ROW BEGIN
		IF NEW.source_event_id LIKE 'e2e-ingest-fail-%' THEN
			UPDATE e2e_ingest_gate SET failures = failures + 1 WHERE id = 1;
			IF (SELECT failures FROM e2e_ingest_gate WHERE id = 1) = 1 THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled notification insert failure';
			END IF;
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_first_notification_insert`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_ingest_gate`)
	}()
	event := s.newEvent("e2e-ingest-fail-"+shortID(), "no HTTP before task commit")
	s.sendEvent(t, event)
	retryMsg := retry.wait(t, event.EventID, 30*time.Second)
	id := s.waitTaskIDByEvent(t, event.EventID, 25*time.Second)
	task := s.wait(t, id, "DELIVERED", 25*time.Second)
	if task.Notification.HTTPAttempts != 1 || s.stats(t).Count != 1 || retryMsg.ReconsumeTimes < 1 {
		t.Fatalf("ingest recovery mismatch: attempts=%d calls=%d reconsume=%d", task.Notification.HTTPAttempts, s.stats(t).Count, retryMsg.ReconsumeTimes)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("notification_insert_failure_retries_before_http", map[string]any{"task_id": id, "http_calls": 1, "mq_reconsume": retryMsg.ReconsumeTimes})
}

func (s *suite) testTerminalPoliciesAndAdmin(t *testing.T) {
	s.reset(t)
	s.fastFail(t, true)
	fast := s.newEvent("e2e-fast-fail-"+shortID(), "fast failure must not call HTTP")
	s.sendEvent(t, fast)
	fastID := s.waitTaskIDByEvent(t, fast.EventID, 15*time.Second)
	fastTask := s.wait(t, fastID, "FAST_FAILED", 15*time.Second)
	if fastTask.Notification.HTTPAttempts != 0 || s.stats(t).Count != 0 {
		t.Fatalf("fast-failed task called HTTP: attempts=%d calls=%d", fastTask.Notification.HTTPAttempts, s.stats(t).Count)
	}
	s.fastFail(t, false)
	newEvent := s.newEvent("e2e-fast-fail-off-"+shortID(), "new event after the switch closes")
	s.sendEvent(t, newEvent)
	newID := s.waitTaskIDByEvent(t, newEvent.EventID, 15*time.Second)
	newTask := s.wait(t, newID, "DELIVERED", 15*time.Second)
	s.sendEvent(t, fast)
	time.Sleep(1200 * time.Millisecond)
	if s.stats(t).Count != 1 || fastTask.Notification.Status != "FAST_FAILED" {
		t.Fatal("closing fast-fail revived an old terminal task")
	}

	expired := s.newEvent("e2e-expired-"+shortID(), "expired body")
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	s.sendEvent(t, expired)
	expiredID := s.waitTaskIDByEvent(t, expired.EventID, 15*time.Second)
	expiredTask := s.wait(t, expiredID, "EXPIRED", 15*time.Second)
	if expiredTask.Notification.HTTPAttempts != 0 || s.stats(t).Count != 1 {
		t.Fatal("expired task should not start an HTTP attempt")
	}

	failed := s.newEvent("e2e-resolve-"+shortID(), "manual resolution only records a conclusion")
	s.configure(t, []int{400}, 0)
	s.sendEvent(t, failed)
	failedID := s.waitTaskIDByEvent(t, failed.EventID, 15*time.Second)
	failedTask := s.wait(t, failedID, "FAILED", 15*time.Second)
	if err := s.resolve(t, failedID, "supplier confirmed permanent rejection"); err != nil {
		t.Fatal(err)
	}
	resolved := s.task(t, failedID)
	if resolved.Notification.Status != "FAILED" || resolved.Notification.Resolution == "" {
		t.Fatalf("manual conclusion changed terminal semantics: status=%s resolution=%q", resolved.Notification.Status, resolved.Notification.Resolution)
	}
	for _, item := range [][2]string{{fastID, fast.TraceID}, {newID, newEvent.TraceID}, {expiredID, expired.TraceID}, {failedID, failed.TraceID}} {
		s.contexts = append(s.contexts, item)
	}
	s.record("fast_fail_expiry_and_admin_resolution", map[string]any{"fast_fail_calls": 0, "new_event_after_close": newTask.Notification.Status, "old_fast_fail_remains": fastTask.Notification.Status, "expired_status": expiredTask.Notification.Status, "manual_resolution_preserves_status": failedTask.Notification.Status})
}

func (s *suite) testResultWriteFailure(t *testing.T, retry *retryObserver) {
	s.reset(t)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered_result`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS e2e_result_gate`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE e2e_result_gate (id INT PRIMARY KEY, writes INT NOT NULL) ENGINE=MyISAM`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO e2e_result_gate (id, writes) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_delivered_result BEFORE UPDATE ON notification FOR EACH ROW BEGIN
		IF NEW.status = 'DELIVERED' THEN
			UPDATE e2e_result_gate SET writes = writes + 1 WHERE id = 1;
			IF (SELECT writes FROM e2e_result_gate WHERE id = 1) <= 3 THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled terminal result save failure';
			END IF;
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_delivered_result`)
		_, _ = s.db.Exec(`DROP TABLE IF EXISTS e2e_result_gate`)
	}()
	s.configure(t, []int{200}, 0)
	event := s.newEvent("e2e-result-write-"+shortID(), "successful remote action must not be repeated")
	s.sendEvent(t, event)
	retryMsg := retry.wait(t, event.EventID, 30*time.Second)
	id := s.waitTaskIDByEvent(t, event.EventID, 20*time.Second)
	task := s.wait(t, id, "UNCERTAIN", 25*time.Second)
	if retryMsg.ReconsumeTimes < 1 || task.Notification.HTTPAttempts != 1 || s.stats(t).Count != 1 || len(task.Attempts) != 1 || task.Attempts[0].State != "UNKNOWN" {
		t.Fatalf("write-result failure repeated or misrecorded request: reconsume=%d attempts=%d calls=%d state=%v", retryMsg.ReconsumeTimes, task.Notification.HTTPAttempts, s.stats(t).Count, task.Attempts)
	}
	s.contexts = append(s.contexts, [2]string{id, event.TraceID})
	s.record("external_success_then_result_write_failure_becomes_uncertain_without_resend", map[string]any{"task_id": id, "http_calls": 1, "mq_reconsume": retryMsg.ReconsumeTimes, "status": task.Notification.Status})
}

func (s *suite) testNativeDLQPaths(t *testing.T, dlq *dlqObserver) {
	s.reset(t)
	badJSON := []byte(`{"schema_version":`)
	s.sendRaw(t, badJSON)
	badMessage := dlq.waitBody(t, badJSON, 2*time.Minute)
	if badMessage.ReconsumeTimes < 2 {
		t.Fatalf("invalid JSON reached DLQ after only %d retries", badMessage.ReconsumeTimes)
	}

	unknown := s.newEvent("e2e-unknown-version-"+shortID(), "unknown schema")
	unknown.SchemaVersion = 99
	unknownRaw, _ := json.Marshal(unknown)
	s.sendRaw(t, unknownRaw)
	unknownMessage := dlq.waitBody(t, unknownRaw, 2*time.Minute)
	if unknownMessage.ReconsumeTimes < 2 {
		t.Fatalf("unknown schema reached DLQ after only %d retries", unknownMessage.ReconsumeTimes)
	}

	conflict := s.newEvent("e2e-conflict-dlq-"+shortID(), "original body")
	s.configure(t, []int{200}, 0)
	s.sendEvent(t, conflict)
	conflictID := s.waitTaskIDByEvent(t, conflict.EventID, 15*time.Second)
	s.wait(t, conflictID, "DELIVERED", 15*time.Second)
	changed := conflict
	changed.Body = "conflicting body must not overwrite or send"
	changedRaw, _ := json.Marshal(changed)
	s.sendRaw(t, changedRaw)
	conflictMessage := dlq.waitBody(t, changedRaw, 2*time.Minute)
	if conflictMessage.ReconsumeTimes < 2 {
		t.Fatalf("event conflict reached DLQ after only %d retries", conflictMessage.ReconsumeTimes)
	}
	var storedBody string
	if err := s.db.QueryRow(`SELECT request_body FROM notification WHERE task_id=?`, conflictID).Scan(&storedBody); err != nil || storedBody != conflict.Body {
		t.Fatalf("conflicting content changed stored task: error=%v", err)
	}
	if s.stats(t).Count != 1 {
		t.Fatalf("conflict content was sent downstream, calls=%d", s.stats(t).Count)
	}
	s.contexts = append(s.contexts, [2]string{conflictID, conflict.TraceID})

	// Permanent attempt-store failure proves execution errors use the same native DLQ path.
	preingested := s.newEvent("e2e-execution-dlq-"+shortID(), "no request while DB attempt insert fails")
	preingestedTask := s.ingestForFaultTest(t, preingested, s.clientID)
	if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_permanent_attempt`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER e2e_fail_permanent_attempt BEFORE INSERT ON delivery_attempt FOR EACH ROW BEGIN
		IF NEW.task_id = '` + preingestedTask + `' THEN
			SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'controlled permanent attempt database failure';
		END IF;
	END`); err != nil {
		t.Fatal(err)
	}
	s.sendEvent(t, preingested)
	failedAttemptMessage := dlq.waitEvent(t, preingested.EventID, 2*time.Minute)
	if failedAttemptMessage.ReconsumeTimes < 2 {
		t.Fatalf("execution error reached DLQ after only %d retries", failedAttemptMessage.ReconsumeTimes)
	}
	_, _ = s.db.Exec(`DROP TRIGGER IF EXISTS e2e_fail_permanent_attempt`)
	var status string
	var attempts int
	if err := s.db.QueryRow(`SELECT status, http_attempts FROM notification WHERE task_id=?`, preingestedTask).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 0 || s.stats(t).Count != 1 {
		t.Fatalf("execution DLQ task changed unexpectedly: status=%s attempts=%d calls=%d", status, attempts, s.stats(t).Count)
	}
	s.record("invalid_version_conflict_and_execution_errors_reach_native_dlq", map[string]any{"invalid_json_reconsume": badMessage.ReconsumeTimes, "unknown_version_reconsume": unknownMessage.ReconsumeTimes, "conflict_reconsume": conflictMessage.ReconsumeTimes, "execution_error_reconsume": failedAttemptMessage.ReconsumeTimes, "conflicting_body_sent": false, "execution_dlq_task_status": status})
}

func (s *suite) testClientScopedEventKeys(t *testing.T) {
	// The database constraint is scoped to (trusted client, source event ID). This test
	// exercises a second configured identity directly without claiming a second MQ auth source.
	event := s.newEvent("e2e-scoped-"+shortID(), "identity scope test")
	idA := s.ingestForFaultTest(t, event, "upstream-a")
	idB := s.ingestForFaultTest(t, event, "upstream-b")
	if idA == idB {
		t.Fatal("same event ID from distinct trusted clients collided")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification WHERE source_event_id=? AND client_id IN ('upstream-a','upstream-b')`, event.EventID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("scoped event rows=%d, error=%v", count, err)
	}
	s.record("event_identity_is_scoped_by_trusted_client", map[string]any{"same_event_id": true, "distinct_clients": 2, "distinct_task_ids": true})
}

func (s *suite) ingestForFaultTest(t *testing.T, event mq.IngressEvent, clientID string) string {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339Nano, event.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	// Use the same production validation and store path; no HTTP send is started here.
	repository, err := store.Open(context.Background(), env(t, "TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service, err := notification.NewService(repository, "http://mock-target:8090")
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := service.Ingest(context.Background(), clientID, event.EventID, event.TraceID, expiresAt,
		notificationInput(event), 256*1024, 16*1024, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func notificationInput(event mq.IngressEvent) notification.Input {
	return notification.Input{
		DestinationID: event.DestinationID,
		Method:        event.Method,
		Headers:       event.Headers,
		Body:          event.Body,
	}
}

func (s *suite) newEvent(eventID, body string) mq.IngressEvent {
	return mq.IngressEvent{
		SchemaVersion: mq.IngressSchemaVersion,
		EventID:       eventID, TraceID: "trace-" + shortID(), DestinationID: "mock-target",
		Method: http.MethodPost, Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body: body, ExpiresAt: time.Now().UTC().Add(90 * time.Second).Truncate(time.Second).Format(time.RFC3339Nano),
	}
}

func (s *suite) sendEvent(t *testing.T, event mq.IngressEvent) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	s.sendRaw(t, body)
}

func (s *suite) sendRaw(t *testing.T, body []byte) {
	t.Helper()
	message := primitive.NewMessage(env(t, "ROCKETMQ_TOPIC"), body)
	if event, err := mq.DecodeEvent(body, 1024*1024); err == nil && event.EventID != "" {
		message.WithKeys([]string{event.EventID})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := s.producer.SendSync(ctx, message)
	if err != nil || result == nil || result.Status != primitive.SendOK {
		t.Fatalf("upstream message was not confirmed: status=%v error=%v", result, err)
	}
}

func (s *suite) waitTaskIDByEvent(t *testing.T, eventID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var id string
		err := s.db.QueryRow(`SELECT task_id FROM notification WHERE client_id=? AND source_event_id=?`, s.clientID, eventID).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("event %s was not committed within %s", eventID, timeout)
	return ""
}

func (s *suite) wait(t *testing.T, taskID, want string, timeout time.Duration) taskView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		view := s.task(t, taskID)
		if view.Notification.Status == want {
			return view
		}
		switch view.Notification.Status {
		case "DELIVERED", "FAILED", "FAST_FAILED", "EXPIRED", "UNCERTAIN":
			t.Fatalf("task reached %s, want %s (reason=%s)", view.Notification.Status, want, view.Notification.Reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach %s within %s", taskID, want, timeout)
	return taskView{}
}

func (s *suite) task(t *testing.T, taskID string) taskView {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, s.api+"/api/v1/notifications/"+taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := s.doAPI(t, request, false)
	defer response.Body.Close()
	var view taskView
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("notification query status=%d", response.StatusCode)
	}
	return view
}

func (s *suite) signedRequest(t *testing.T, method, path string, body []byte, admin bool) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, s.api+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if admin {
		request.Header.Set("Authorization", "Bearer "+s.admin)
		if _, err := signature.SignRequest(request, s.adminKeyID, s.adminKey, time.Now()); err != nil {
			t.Fatal(err)
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+s.token)
		if _, err := signature.SignRequest(request, s.clientKeyID, s.clientKey, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return request
}

func (s *suite) doAPI(t *testing.T, request *http.Request, admin bool) *http.Response {
	t.Helper()
	if admin {
		request.Header.Set("Authorization", "Bearer "+s.admin)
		if _, err := signature.SignRequest(request, s.adminKeyID, s.adminKey, time.Now()); err != nil {
			t.Fatal(err)
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+s.token)
		if _, err := signature.SignRequest(request, s.clientKeyID, s.clientKey, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (s *suite) fastFail(t *testing.T, enabled bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"enabled": enabled, "reason": "integration controlled switch"})
	request, _ := http.NewRequest(http.MethodPatch, s.api+"/api/v1/admin/destinations/mock-target/fast-fail", bytes.NewReader(body))
	response := s.doAPI(t, request, true)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fast-fail update status=%d", response.StatusCode)
	}
}

func (s *suite) resolve(t *testing.T, taskID, conclusion string) error {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"conclusion": conclusion})
	request, _ := http.NewRequest(http.MethodPost, s.api+"/api/v1/admin/notifications/"+taskID+"/resolve", bytes.NewReader(body))
	response := s.doAPI(t, request, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("resolve status=%d", response.StatusCode)
	}
	return nil
}

func (s *suite) reset(t *testing.T) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, s.mock+"/control/reset", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("mock reset status=%d", response.StatusCode)
	}
}

func (s *suite) configure(t *testing.T, statuses []int, delayMS int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"statuses": statuses, "delay_ms": delayMS, "response_body": "controlled"})
	request, _ := http.NewRequest(http.MethodPost, s.mock+"/control/configure", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("mock configure status=%d", response.StatusCode)
	}
}

func (s *suite) stats(t *testing.T) statsView {
	t.Helper()
	response, err := http.Get(s.mock + "/control/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var stats statsView
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	return stats
}

func (s *suite) observeRetries(t *testing.T) *retryObserver {
	t.Helper()
	resolver, err := resolveNameServer(env(t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName("notification_retry_audit"),
		consumer.WithNsResolver(primitive.NewPassthroughResolver([]string{resolver})),
		consumer.WithConsumeMessageBatchMaxSize(1), consumer.WithPullBatchSize(1), consumer.WithConsumeGoroutineNums(1), consumer.WithMaxReconsumeTimes(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	observer := &retryObserver{messages: make(chan *primitive.MessageExt, 64), consumer: client}
	if err := client.Subscribe("%RETRY%"+env(t, "TEST_CONSUMER_GROUP"), consumer.MessageSelector{}, func(_ context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
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
	t.Cleanup(func() { _ = client.Shutdown() })
	return observer
}

func (s *suite) observeDLQ(t *testing.T) *dlqObserver {
	t.Helper()
	resolver, err := resolveNameServer(env(t, "ROCKETMQ_NAMESRV"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName("notification_dlq_audit"),
		consumer.WithNsResolver(primitive.NewPassthroughResolver([]string{resolver})),
		consumer.WithConsumeMessageBatchMaxSize(1), consumer.WithPullBatchSize(1), consumer.WithConsumeGoroutineNums(1), consumer.WithMaxReconsumeTimes(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	observer := &dlqObserver{messages: make(chan *primitive.MessageExt, 64), consumer: client}
	topic := "%DLQ%" + env(t, "TEST_CONSUMER_GROUP")
	if err := client.Subscribe(topic, consumer.MessageSelector{}, func(_ context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
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
	t.Cleanup(func() { _ = client.Shutdown() })
	return observer
}

func (o *retryObserver) wait(t *testing.T, eventID string, timeout time.Duration) *primitive.MessageExt {
	t.Helper()
	return waitRocketEvent(t, o.messages, eventID, timeout)
}

func (o *dlqObserver) waitEvent(t *testing.T, eventID string, timeout time.Duration) *primitive.MessageExt {
	t.Helper()
	return waitRocketEvent(t, o.messages, eventID, timeout)
}

func (o *dlqObserver) waitBody(t *testing.T, want []byte, timeout time.Duration) *primitive.MessageExt {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case message := <-o.messages:
			if bytes.Equal(message.Body, want) {
				return message
			}
		case <-timer.C:
			t.Fatalf("message did not arrive on native DLQ within %s", timeout)
		}
	}
}

func waitRocketEvent(t *testing.T, messages <-chan *primitive.MessageExt, eventID string, timeout time.Duration) *primitive.MessageExt {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case message := <-messages:
			var event mq.IngressEvent
			if json.Unmarshal(message.Body, &event) == nil && event.EventID == eventID {
				return message
			}
		case <-timer.C:
			t.Fatalf("event %s did not arrive on RocketMQ retry/DLQ topic within %s", eventID, timeout)
		}
	}
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

func (s *suite) saveContexts() {
	path := os.Getenv("TEST_CONTROL_DIR")
	if path == "" {
		return
	}
	var content strings.Builder
	for _, pair := range s.contexts {
		fmt.Fprintf(&content, "%s\t%s\n", pair[0], pair[1])
	}
	_ = os.WriteFile(filepath.Join(path, "log-context.tsv"), []byte(content.String()), 0600)
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("control marker was not found: %s", filepath.Base(path))
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

func shortID() string { return fmt.Sprintf("%x", time.Now().UnixNano()) }

func safeOutput(output []byte) string {
	if len(output) > 300 {
		output = output[:300]
	}
	return strings.TrimSpace(string(output))
}

func env(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required", key)
	}
	return value
}
