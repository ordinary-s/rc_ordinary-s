package notification

import (
	"bytes"
	"net/http"
	"testing"
)

func TestValidateInputPreservesBodyAndCanonicalizesHeaders(t *testing.T) {
	input := Input{
		DestinationID: "target-a",
		Method:        " post ",
		Headers:       map[string]string{"content-type": "application/xml", "X-Format": "raw"},
		Body:          "<通知>ok</通知>",
	}
	got, body, _, err := validateInput(input, 1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost {
		t.Fatalf("method = %q", got.Method)
	}
	if got.Headers["Content-Type"] != "application/xml" || got.Headers["X-Format"] != "raw" {
		t.Fatalf("headers were not canonicalized: %#v", got.Headers)
	}
	if !bytes.Equal(body, []byte(input.Body)) {
		t.Fatalf("body changed: %q", body)
	}
}

func TestFingerprintIncludesTargetMethodHeadersAndExactBody(t *testing.T) {
	headers := map[string]string{"Content-Type": "application/xml"}
	first, err := fingerprint("a", "POST", headers, []byte("<x/>"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		target string
		method string
		header string
		body   string
	}{
		{"b", "POST", "application/xml", "<x/>"},
		{"a", "PUT", "application/xml", "<x/>"},
		{"a", "POST", "text/xml", "<x/>"},
		{"a", "POST", "application/xml", "<x />"},
	}
	for _, test := range cases {
		value, err := fingerprint(test.target, test.method, map[string]string{"Content-Type": test.header}, []byte(test.body))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(first, value) {
			t.Fatalf("fingerprint ignored changed request field: %+v", test)
		}
	}
}

func TestCanonicalHeadersRejectsAmbiguousAndHopByHopHeaders(t *testing.T) {
	_, err := canonicalHeaders(map[string]string{"x-test": "a", "X-Test": "b"}, 1024)
	if err == nil {
		t.Fatal("expected duplicate canonical header rejection")
	}
	_, err = canonicalHeaders(map[string]string{"Connection": "keep-alive"}, 1024)
	if err == nil {
		t.Fatal("expected hop-by-hop header rejection")
	}
}
