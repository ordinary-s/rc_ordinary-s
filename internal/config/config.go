package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr            string
	DBDSN               string
	NameServer          string
	BrokerAddress       string
	Topic               string
	ProducerGroup       string
	ConsumerGroup       string
	ClientID            string
	ClientToken         string
	AdminToken          string
	DestinationID       string
	DestinationURL      string
	AllowedTarget       string
	ConsumerConcurrency int
	MaxReconsume        int32
	HTTPTimeout         time.Duration
	TaskTTL             time.Duration
	Backoffs            []time.Duration
	OutboxPoll          time.Duration
	MaxBodyBytes        int64
	MaxHeaderBytes      int
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:            env("HTTP_ADDR", ":8080"),
		DBDSN:               env("DB_DSN", "notification_app:notification-app-local@tcp(mysql:3306)/notifications?parseTime=true&loc=UTC&charset=utf8mb4"),
		NameServer:          env("ROCKETMQ_NAMESRV", "namesrv:9876"),
		BrokerAddress:       env("ROCKETMQ_BROKER_ADDR", "broker:10911"),
		Topic:               env("ROCKETMQ_TOPIC", "notification_delivery"),
		ProducerGroup:       env("ROCKETMQ_PRODUCER_GROUP", "notification_outbox_publisher"),
		ConsumerGroup:       env("ROCKETMQ_CONSUMER_GROUP", "notification_delivery_workers"),
		ClientID:            env("CLIENT_ID", "demo-client"),
		ClientToken:         env("CLIENT_TOKEN", "demo-client-token"),
		AdminToken:          env("ADMIN_TOKEN", "demo-admin-token"),
		DestinationID:       env("DESTINATION_ID", "mock-target"),
		DestinationURL:      env("DESTINATION_URL", "http://mock-target:8090/notify"),
		AllowedTarget:       env("ALLOWED_TARGET", "http://mock-target:8090"),
		ConsumerConcurrency: 4,
		MaxReconsume:        8,
		HTTPTimeout:         10 * time.Second,
		TaskTTL:             30 * time.Minute,
		Backoffs:            []time.Duration{10 * time.Second, 20 * time.Second},
		OutboxPoll:          time.Second,
		MaxBodyBytes:        256 * 1024,
		MaxHeaderBytes:      16 * 1024,
	}
	var err error
	if c.ConsumerConcurrency, err = intEnv("CONSUMER_CONCURRENCY", c.ConsumerConcurrency); err != nil {
		return Config{}, err
	}
	if c.MaxReconsume, err = int32Env("MQ_MAX_RECONSUME", c.MaxReconsume); err != nil {
		return Config{}, err
	}
	if c.HTTPTimeout, err = durationEnv("HTTP_TIMEOUT", c.HTTPTimeout); err != nil {
		return Config{}, err
	}
	if c.TaskTTL, err = durationEnv("TASK_TTL", c.TaskTTL); err != nil {
		return Config{}, err
	}
	if c.OutboxPoll, err = durationEnv("OUTBOX_POLL", c.OutboxPoll); err != nil {
		return Config{}, err
	}
	if c.Backoffs, err = durationListEnv("HTTP_BACKOFFS", c.Backoffs); err != nil {
		return Config{}, err
	}
	if c.ConsumerConcurrency < 1 || c.ConsumerConcurrency > 64 {
		return Config{}, fmt.Errorf("CONSUMER_CONCURRENCY must be between 1 and 64")
	}
	if c.MaxReconsume < 0 || c.MaxReconsume > 32 {
		return Config{}, fmt.Errorf("MQ_MAX_RECONSUME must be between 0 and 32")
	}
	if len(c.Backoffs) != 2 {
		return Config{}, fmt.Errorf("HTTP_BACKOFFS must contain exactly two delays")
	}
	for _, d := range c.Backoffs {
		if d < 0 {
			return Config{}, fmt.Errorf("HTTP_BACKOFFS values must not be negative")
		}
	}
	if c.HTTPTimeout <= 0 || c.TaskTTL <= 0 || c.OutboxPoll <= 0 {
		return Config{}, fmt.Errorf("timeouts and polling interval must be positive")
	}
	if strings.TrimSpace(c.ClientToken) == "" || strings.TrimSpace(c.AdminToken) == "" {
		return Config{}, fmt.Errorf("client and admin tokens must be configured")
	}
	return c, nil
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func intEnv(key string, fallback int) (int, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return result, nil
}

func int32Env(key string, fallback int32) (int32, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}
	result, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return int32(result), nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}
	result, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return result, nil
}

func durationListEnv(key string, fallback []time.Duration) ([]time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}
	parts := strings.Split(value, ",")
	result := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		delay, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		result = append(result, delay)
	}
	return result, nil
}
