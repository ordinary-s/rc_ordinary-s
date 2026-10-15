package mq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/ordinary-s/rc_ordinary-s/internal/config"
)

const IngressSchemaVersion = 1

// IngressEvent is the versioned upstream contract carried by RocketMQ.
type IngressEvent struct {
	SchemaVersion int               `json:"schema_version"`
	EventID       string            `json:"event_id"`
	TraceID       string            `json:"trace_id"`
	DestinationID string            `json:"destination_id"`
	Method        string            `json:"method"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          string            `json:"body"`
	ExpiresAt     string            `json:"expires_at"`
}

// ProtocolError identifies a message that must follow the native retry/DLQ path.
type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return e.Reason }
func (e *ProtocolError) Code() string  { return e.Reason }

type ProcessFunc func(context.Context, IngressEvent, int) error

type Client struct {
	cfg      config.Config
	logger   *slog.Logger
	consumer rocketmq.PushConsumer
	started  atomic.Bool
}

func New(cfg config.Config, logger *slog.Logger) (*Client, error) {
	address, err := ResolveNameServer(cfg.NameServer)
	if err != nil {
		return nil, err
	}
	resolver := primitive.NewPassthroughResolver([]string{address})
	c, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName(cfg.ConsumerGroup),
		consumer.WithNsResolver(resolver),
		consumer.WithConsumerModel(consumer.Clustering),
		consumer.WithConsumeMessageBatchMaxSize(1),
		consumer.WithPullBatchSize(1),
		consumer.WithConsumeGoroutineNums(cfg.ConsumerConcurrency),
		consumer.WithMaxReconsumeTimes(cfg.MaxReconsume),
		consumer.WithConsumeTimeout(cfg.MQConsumeTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("create RocketMQ consumer: %w", err)
	}
	return &Client{cfg: cfg, logger: logger, consumer: c}, nil
}

func DecodeEvent(body []byte, maxBytes int) (IngressEvent, error) {
	if len(body) == 0 || len(body) > maxBytes || !utf8.Valid(body) {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_SIZE_INVALID"}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var event IngressEvent
	if err := decoder.Decode(&event); err != nil {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_JSON_INVALID"}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_JSON_INVALID"}
	}
	if event.SchemaVersion != IngressSchemaVersion {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_SCHEMA_VERSION_UNSUPPORTED"}
	}
	if event.EventID == "" || event.TraceID == "" || event.DestinationID == "" || event.Method == "" || event.ExpiresAt == "" {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_REQUIRED_FIELD_MISSING"}
	}
	if !strings.HasSuffix(event.ExpiresAt, "Z") {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_EXPIRY_MUST_BE_UTC"}
	}
	if _, err := time.Parse(time.RFC3339Nano, event.ExpiresAt); err != nil {
		return IngressEvent{}, &ProtocolError{Reason: "MESSAGE_EXPIRY_INVALID"}
	}
	return event, nil
}

// ResolveNameServer resolves the NameServer host to an IP for the v2 SDK resolver.
func ResolveNameServer(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("parse RocketMQ NameServer address: %w", err)
	}
	if net.ParseIP(host) != nil {
		return address, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("resolve RocketMQ NameServer service %q: %w", host, err)
	}
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			return net.JoinHostPort(ipv4.String(), port), nil
		}
	}
	if len(ips) > 0 {
		return net.JoinHostPort(ips[0].String(), port), nil
	}
	return "", fmt.Errorf("resolve RocketMQ NameServer service %q: no IP address returned", host)
}

func (c *Client) Start(runCtx context.Context, process ProcessFunc) error {
	if err := c.consumer.Subscribe(c.cfg.Topic, consumer.MessageSelector{}, func(ctx context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
		if len(messages) != 1 {
			return consumer.ConsumeRetryLater, errors.New("expected single-message callback")
		}
		message := messages[0]
		event, err := DecodeEvent(message.Body, c.cfg.MaxMessageBytes)
		if err != nil {
			c.logger.Warn("upstream message rejected", "code", protocolCode(err), "mq_reconsume", message.ReconsumeTimes)
			return consumer.ConsumeRetryLater, err
		}
		workCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(runCtx, cancel)
		defer func() {
			stop()
			cancel()
		}()
		if err := process(workCtx, event, int(message.ReconsumeTimes)); err != nil {
			code := "EXECUTION_ERROR"
			if value := protocolCode(err); value != "" {
				code = value
			}
			c.logger.Warn("upstream message not completed", "event_id", event.EventID, "trace_id", event.TraceID, "mq_reconsume", message.ReconsumeTimes, "code", code, "error_type", fmt.Sprintf("%T", err))
			return consumer.ConsumeRetryLater, errors.New("notification message was not completed")
		}
		return consumer.ConsumeSuccess, nil
	}); err != nil {
		return fmt.Errorf("subscribe RocketMQ topic %s: %w", c.cfg.Topic, err)
	}
	if err := c.consumer.Start(); err != nil {
		_ = c.consumer.Shutdown()
		return fmt.Errorf("start RocketMQ consumer: %w", err)
	}
	c.started.Store(true)
	return nil
}

func protocolCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

func (c *Client) Ready(ctx context.Context, brokerAddress string) error {
	if !c.started.Load() {
		return errors.New("RocketMQ consumer is not started")
	}
	for _, address := range []string{c.cfg.NameServer, brokerAddress} {
		dialer := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("RocketMQ endpoint %s unavailable: %w", address, err)
		}
		_ = conn.Close()
	}
	return nil
}

func (c *Client) Shutdown() error {
	c.started.Store(false)
	return c.consumer.Shutdown()
}
