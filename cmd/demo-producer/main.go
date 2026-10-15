package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	eventID := flag.String("event-id", "", "stable upstream event ID; generated when omitted")
	traceID := flag.String("trace-id", "", "trace identifier; generated when omitted")
	destinationID := flag.String("destination", "mock-target", "configured destination ID")
	method := flag.String("method", "POST", "HTTP method")
	contentType := flag.String("content-type", "text/plain; charset=utf-8", "request Content-Type")
	bodyText := flag.String("body", "demo notification", "raw UTF-8 request body")
	expiresAtText := flag.String("expires-at", "", "absolute UTC RFC3339 expiry; defaults to 30 minutes from now")
	count := flag.Int("count", 1, "send the same event this many times to demonstrate redelivery")
	flag.Parse()
	if *count < 1 || *count > 20 {
		return fmt.Errorf("-count must be between 1 and 20")
	}
	if *eventID == "" {
		*eventID = fmt.Sprintf("demo-%d", time.Now().UTC().UnixNano())
	}
	if *traceID == "" {
		*traceID = fmt.Sprintf("trace-%d", time.Now().UTC().UnixNano())
	}
	expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	if *expiresAtText != "" {
		if len(*expiresAtText) == 0 || (*expiresAtText)[len(*expiresAtText)-1] != 'Z' {
			return fmt.Errorf("-expires-at must end in Z and use UTC")
		}
		parsed, err := time.Parse(time.RFC3339Nano, *expiresAtText)
		if err != nil {
			return fmt.Errorf("parse -expires-at: %w", err)
		}
		expiresAt = parsed.UTC()
	}
	event := mq.IngressEvent{
		SchemaVersion: mq.IngressSchemaVersion,
		EventID:       *eventID,
		TraceID:       *traceID,
		DestinationID: *destinationID,
		Method:        *method,
		Headers:       map[string]string{"Content-Type": *contentType},
		Body:          *bodyText,
		ExpiresAt:     expiresAt.Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode ingress event: %w", err)
	}
	nameServer := env("ROCKETMQ_NAMESRV", "namesrv:9876")
	nameServer, err = mq.ResolveNameServer(nameServer)
	if err != nil {
		return fmt.Errorf("resolve RocketMQ NameServer: %w", err)
	}
	topic := env("ROCKETMQ_TOPIC", "notification_ingress_v1")
	client, err := rocketmq.NewProducer(
		producer.WithGroupName("notification_demo_producer"),
		producer.WithNsResolver(primitive.NewPassthroughResolver([]string{nameServer})),
		producer.WithRetry(0),
		producer.WithSendMsgTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("create demo producer: %w", err)
	}
	if err := client.Start(); err != nil {
		return fmt.Errorf("start demo producer: %w", err)
	}
	defer client.Shutdown()
	for i := 0; i < *count; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		message := primitive.NewMessage(topic, body)
		message.WithKeys([]string{event.EventID})
		result, sendErr := client.SendSync(ctx, message)
		cancel()
		if sendErr != nil {
			return fmt.Errorf("send event %s: %w", event.EventID, sendErr)
		}
		if result == nil || result.Status != primitive.SendOK {
			return fmt.Errorf("broker did not confirm event %s: status=%v", event.EventID, result)
		}
	}
	fmt.Printf("confirmed topic=%s event_id=%s sends=%d expires_at=%s\n", topic, event.EventID, *count, event.ExpiresAt)
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
