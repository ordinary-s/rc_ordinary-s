package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPSAddr            string
	TLSCertFile          string
	TLSKeyFile           string
	DBDSN                string
	NameServer           string
	BrokerAddress        string
	Topic                string
	ConsumerGroup        string
	ClientID             string
	MQTrustedClientID    string
	ClientToken          string
	AdminToken           string
	AdminActor           string
	ClientSignatureKeyID string
	ClientSignatureKey   []byte
	AdminSignatureKeyID  string
	AdminSignatureKey    []byte
	DestinationID        string
	DestinationURL       string
	AllowedTarget        string
	ConsumerConcurrency  int
	MaxReconsume         int32
	HTTPTimeout          time.Duration
	DeliveryBudget       time.Duration
	MQConsumeTimeout     time.Duration
	TaskTTL              time.Duration
	Backoffs             []time.Duration
	MaxBodyBytes         int64
	MaxHeaderBytes       int
	MaxMessageBytes      int
}

func Load() (Config, error) {
	c := Config{
		HTTPSAddr:            env("HTTPS_ADDR", ":8443"),
		TLSCertFile:          env("TLS_CERT_FILE", "/certs/server.crt"),
		TLSKeyFile:           env("TLS_KEY_FILE", "/certs/server.key"),
		DBDSN:                env("DB_DSN", "notification_app:notification-app-local@tcp(mysql:3306)/notifications?parseTime=true&loc=UTC&charset=utf8mb4"),
		NameServer:           env("ROCKETMQ_NAMESRV", "namesrv:9876"),
		BrokerAddress:        env("ROCKETMQ_BROKER_ADDR", "broker:10911"),
		Topic:                env("ROCKETMQ_TOPIC", "notification_ingress_v1"),
		ConsumerGroup:        env("ROCKETMQ_CONSUMER_GROUP", "notification_ingress_workers_v1"),
		ClientID:             env("CLIENT_ID", "demo-upstream"),
		MQTrustedClientID:    env("MQ_TRUSTED_CLIENT_ID", env("CLIENT_ID", "demo-upstream")),
		ClientToken:          env("CLIENT_TOKEN", "demo-client-token"),
		AdminToken:           env("ADMIN_TOKEN", "demo-admin-token"),
		AdminActor:           env("ADMIN_ACTOR", "local-admin"),
		ClientSignatureKeyID: env("CLIENT_SIGNATURE_KEY_ID", "demo-client-v1"),
		AdminSignatureKeyID:  env("ADMIN_SIGNATURE_KEY_ID", "demo-admin-v1"),
		DestinationID:        env("DESTINATION_ID", "mock-target"),
		DestinationURL:       env("DESTINATION_URL", "http://mock-target:8090/notify"),
		AllowedTarget:        env("ALLOWED_TARGET", "http://mock-target:8090"),
		ConsumerConcurrency:  4,
		MaxReconsume:         8,
		HTTPTimeout:          10 * time.Second,
		DeliveryBudget:       90 * time.Second,
		MQConsumeTimeout:     2 * time.Minute,
		TaskTTL:              30 * time.Minute,
		Backoffs:             []time.Duration{10 * time.Second, 20 * time.Second},
		MaxBodyBytes:         256 * 1024,
		MaxHeaderBytes:       16 * 1024,
		MaxMessageBytes:      1024 * 1024,
	}
	var err error
	if c.ClientSignatureKey, err = secretEnv("CLIENT_SIGNATURE_SECRET_HEX", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"); err != nil {
		return Config{}, err
	}
	if c.AdminSignatureKey, err = secretEnv("ADMIN_SIGNATURE_SECRET_HEX", "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"); err != nil {
		return Config{}, err
	}
	if c.ConsumerConcurrency, err = intEnv("CONSUMER_CONCURRENCY", c.ConsumerConcurrency); err != nil {
		return Config{}, err
	}
	if c.MaxReconsume, err = int32Env("MQ_MAX_RECONSUME", c.MaxReconsume); err != nil {
		return Config{}, err
	}
	if c.HTTPTimeout, err = durationEnv("HTTP_TIMEOUT", c.HTTPTimeout); err != nil {
		return Config{}, err
	}
	if c.DeliveryBudget, err = durationEnv("DELIVERY_BUDGET", c.DeliveryBudget); err != nil {
		return Config{}, err
	}
	if c.MQConsumeTimeout, err = durationEnv("MQ_CONSUME_TIMEOUT", c.MQConsumeTimeout); err != nil {
		return Config{}, err
	}
	if c.TaskTTL, err = durationEnv("TASK_TTL", c.TaskTTL); err != nil {
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
	if c.HTTPTimeout <= 0 || c.DeliveryBudget <= 0 || c.MQConsumeTimeout <= 0 || c.TaskTTL <= 0 {
		return Config{}, fmt.Errorf("timeouts must be positive")
	}
	minimumDeliveryBudget := 3*c.HTTPTimeout + c.Backoffs[0] + c.Backoffs[1] + 950*time.Millisecond + 5*time.Second
	if c.DeliveryBudget < minimumDeliveryBudget {
		return Config{}, fmt.Errorf("DELIVERY_BUDGET must cover three HTTP timeouts, both backoffs, result persistence, and margin (at least %s)", minimumDeliveryBudget)
	}
	if c.DeliveryBudget+5*time.Second > c.MQConsumeTimeout {
		return Config{}, fmt.Errorf("MQ_CONSUME_TIMEOUT must exceed DELIVERY_BUDGET by at least 5s")
	}
	if strings.TrimSpace(c.ClientToken) == "" || strings.TrimSpace(c.AdminToken) == "" || strings.TrimSpace(c.AdminActor) == "" || !validActor(c.MQTrustedClientID) {
		return Config{}, fmt.Errorf("client token, admin token, admin actor, and a valid MQ_TRUSTED_CLIENT_ID must be configured")
	}
	if !validActor(c.ClientSignatureKeyID) || !validActor(c.AdminSignatureKeyID) || c.ClientSignatureKeyID == c.AdminSignatureKeyID {
		return Config{}, fmt.Errorf("signature key IDs must be distinct 1-64 character identifiers")
	}
	if c.HTTPSAddr == "" || c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return Config{}, fmt.Errorf("HTTPS_ADDR, TLS_CERT_FILE, and TLS_KEY_FILE must be configured")
	}
	if !validActor(c.AdminActor) {
		return Config{}, fmt.Errorf("ADMIN_ACTOR must be 1-64 letters, digits, dots, underscores, or hyphens")
	}
	return c, nil
}

func secretEnv(key, fallback string) ([]byte, error) {
	value := env(key, fallback)
	secret, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be hexadecimal", key)
	}
	if len(secret) < 32 {
		return nil, fmt.Errorf("%s must contain at least 32 bytes", key)
	}
	return secret, nil
}

func validActor(actor string) bool {
	if actor == "" || len(actor) > 64 {
		return false
	}
	for _, r := range actor {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
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
