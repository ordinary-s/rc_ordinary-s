// Command demo-client sends a signed request to the local Compose API.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/signature"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	apiURL := env("TEST_API_URL", "https://app:8443")
	caPEM, err := os.ReadFile(env("TEST_TLS_CA_FILE", "/certs/ca.crt"))
	if err != nil {
		return fmt.Errorf("read local CA certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("local CA certificate is invalid")
	}
	secret, err := hex.DecodeString(env("TEST_CLIENT_SIGNATURE_SECRET_HEX", ""))
	if err != nil || len(secret) < 32 {
		return fmt.Errorf("client signature secret must be at least 32 bytes of hexadecimal")
	}
	keyID := env("TEST_CLIENT_SIGNATURE_KEY_ID", "")
	token := env("TEST_CLIENT_TOKEN", "")
	if keyID == "" || token == "" {
		return fmt.Errorf("client signature key ID and bearer token are required")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	requestBody, err := json.Marshal(map[string]any{
		"destination_id": "mock-target",
		"method":         http.MethodPost,
		"headers":        map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		"body":           "signed local demo notification",
	})
	if err != nil {
		return fmt.Errorf("encode demo request: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, apiURL+"/api/v1/notifications", bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("create demo request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	idempotencyKey, err := signature.NewNonce()
	if err != nil {
		return err
	}
	request.Header.Set("Idempotency-Key", "demo-"+idempotencyKey)
	if _, err := signature.SignRequest(request, keyID, secret, time.Now()); err != nil {
		return fmt.Errorf("sign demo request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send signed request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("read API response: %w", err)
	}
	if len(responseBody) > 4096 {
		return fmt.Errorf("API response exceeded 4096 bytes")
	}
	fmt.Printf("status=%d tls=%s response=%s", response.StatusCode, tls.VersionName(response.TLS.Version), responseBody)
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("API returned status %d", response.StatusCode)
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
