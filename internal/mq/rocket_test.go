package mq

import (
	"strings"
	"testing"
)

func TestDecodeEventValidatesVersionedUTCEnvelope(t *testing.T) {
	body := `{"schema_version":1,"event_id":"event-1","trace_id":"trace-1","destination_id":"d1","method":"POST","headers":{"Content-Type":"text/plain"},"body":"你好","expires_at":"2030-01-02T03:04:05Z"}`
	event, err := DecodeEvent([]byte(body), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != 1 || event.EventID != "event-1" || event.Body != "你好" {
		t.Fatalf("decoded envelope mismatch: %#v", event)
	}
}

func TestDecodeEventRejectsMalformedEnvelope(t *testing.T) {
	valid := `{"schema_version":1,"event_id":"event-1","trace_id":"trace-1","destination_id":"d1","method":"POST","body":"x","expires_at":"2030-01-02T03:04:05Z"}`
	cases := map[string]string{
		"unknown version": strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		"unknown field":   strings.Replace(valid, `"body":"x"`, `"body":"x","surprise":true`, 1),
		"non UTC":         strings.Replace(valid, `2030-01-02T03:04:05Z`, `2030-01-02T03:04:05+00:00`, 1),
		"trailing JSON":   valid + `{}`,
		"missing field":   strings.Replace(valid, `"trace_id":"trace-1",`, ``, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEvent([]byte(body), 1024); err == nil {
				t.Fatal("DecodeEvent accepted malformed message")
			}
		})
	}
	if _, err := DecodeEvent([]byte(valid), len(valid)-1); err == nil {
		t.Fatal("DecodeEvent accepted oversized message")
	}
	if _, err := DecodeEvent([]byte{0xff, 0xfe}, 1024); err == nil {
		t.Fatal("DecodeEvent accepted invalid UTF-8")
	}
}
