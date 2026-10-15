package signature

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerifyRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := bytes.Repeat([]byte{0x42}, 32)
	req, err := http.NewRequest(http.MethodPost, "https://notify.example/api/v1/notifications?mode=fast", strings.NewReader(`{"body":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Idempotency-Key", "sample-1")
	if _, err := SignRequestWithNonce(req, "client-v1", key, now, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyRequest(req, map[string][]byte{"client-v1": key}, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.KeyID != "client-v1" || claims.Nonce != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
}

func TestVerifyRequestRejectsBodyTamperingAndExpiredSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := bytes.Repeat([]byte{0x42}, 32)
	newRequest := func(at time.Time) *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://notify.example/api/v1/notifications", strings.NewReader(`{"body":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Idempotency-Key", "sample-1")
		if _, err := SignRequestWithNonce(req, "client-v1", key, at, "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		return req
	}

	tampered := newRequest(now)
	tampered.Body = io.NopCloser(strings.NewReader(`{"body":"other"}`))
	if _, err := VerifyRequest(tampered, map[string][]byte{"client-v1": key}, now); err == nil {
		t.Fatal("tampered body was accepted")
	}
	if _, err := VerifyRequest(newRequest(now.Add(-10*time.Minute)), map[string][]byte{"client-v1": key}, now); err == nil {
		t.Fatal("expired signature was accepted")
	}
}
