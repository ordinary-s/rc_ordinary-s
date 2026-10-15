// Package signature implements the API's RFC 9421 HMAC request signature policy.
package signature

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	htmsig "github.com/lestrrat-go/htmsig"
	"github.com/lestrrat-go/htmsig/component"
	"github.com/lestrrat-go/htmsig/input"
)

const (
	algorithm      = "hmac-sha256"
	label          = "sig1"
	maxValidity    = 5 * time.Minute
	maxFutureSkew  = 30 * time.Second
	maxBodyForSign = 1024 * 1024
)

var ErrInvalid = errors.New("invalid HTTP message signature")

// Claims contains the verified, replay-protected fields needed by the API.
type Claims struct {
	KeyID   string
	Nonce   string
	Expires time.Time
}

// SignRequest adds an RFC 9530 Content-Digest and RFC 9421 HMAC signature.
func SignRequest(req *http.Request, keyID string, key []byte, now time.Time) (Claims, error) {
	nonce, err := NewNonce()
	if err != nil {
		return Claims{}, err
	}
	return SignRequestWithNonce(req, keyID, key, now, nonce)
}

// SignRequestWithNonce allows deterministic callers to sign a request for tests.
func SignRequestWithNonce(req *http.Request, keyID string, key []byte, now time.Time, nonce string) (Claims, error) {
	if req == nil || keyID == "" || len(key) < 32 || !validNonce(nonce) {
		return Claims{}, ErrInvalid
	}
	body, err := readAndRestoreBody(req, maxBodyForSign)
	if err != nil {
		return Claims{}, fmt.Errorf("read request body: %w", err)
	}
	digest := sha256.Sum256(body)
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
	components := []component.Identifier{
		component.Method(), component.Authority(), component.TargetURI(), component.New("@path"),
		component.New("authorization"), component.New("content-digest"),
	}
	if req.Header.Get("Idempotency-Key") != "" {
		components = append(components, component.New("idempotency-key"))
	}
	definition, err := input.NewDefinitionBuilder().
		Label(label).KeyID(keyID).Algorithm(algorithm).Components(components...).
		CreatedTime(now).ExpiresTime(now.Add(maxValidity)).Nonce(nonce).Build()
	if err != nil {
		return Claims{}, fmt.Errorf("build signature input: %w", err)
	}
	value := (&input.Value{}).AddDefinition(definition)
	ctx := component.WithRequestInfoFromHTTP(context.Background(), req)
	if err := htmsig.SignRequest(ctx, req.Header, value, key); err != nil {
		return Claims{}, fmt.Errorf("sign request: %w", err)
	}
	return Claims{KeyID: keyID, Nonce: nonce, Expires: now.Add(maxValidity)}, nil
}

// VerifyRequest checks the signature metadata, covered components, body digest, and MAC.
func VerifyRequest(req *http.Request, keys map[string][]byte, now time.Time) (Claims, error) {
	if req == nil || len(keys) == 0 || !singleHeader(req.Header, htmsig.SignatureInputHeader) || !singleHeader(req.Header, htmsig.SignatureHeader) || !singleHeader(req.Header, "Content-Digest") {
		return Claims{}, ErrInvalid
	}
	if strings.Contains(req.Header.Get(htmsig.SignatureHeader), ",") {
		return Claims{}, ErrInvalid
	}
	value, err := input.Parse([]byte(req.Header.Get(htmsig.SignatureInputHeader)))
	if err != nil || value.Len() != 1 {
		return Claims{}, ErrInvalid
	}
	definition := value.Definitions()[0]
	if definition.Label() != label || definition.Algorithm() != algorithm || !validKeyID(definition.KeyID()) {
		return Claims{}, ErrInvalid
	}
	if !hasRequiredComponents(definition.Components(), req.Method == http.MethodPost && req.URL.Path == "/api/v1/notifications") {
		return Claims{}, ErrInvalid
	}
	created, hasCreated := definition.Created()
	expires, hasExpires := definition.Expires()
	nonce, hasNonce := definition.Nonce()
	nowUnix := now.Unix()
	if !hasCreated || !hasExpires || !hasNonce || !validNonce(nonce) || created > nowUnix+int64(maxFutureSkew.Seconds()) || created < nowUnix-int64(maxValidity.Seconds()) || expires <= nowUnix || expires <= created || expires-created > int64(maxValidity.Seconds()) || expires > nowUnix+int64(maxValidity.Seconds()) {
		return Claims{}, ErrInvalid
	}
	key, ok := keys[definition.KeyID()]
	if !ok || len(key) < 32 {
		return Claims{}, ErrInvalid
	}
	body, err := readAndRestoreBody(req, maxBodyForSign)
	if err != nil || !validContentDigest(req.Header.Get("Content-Digest"), body) {
		return Claims{}, ErrInvalid
	}
	resolver := singleKeyResolver{keyID: definition.KeyID(), key: key}
	ctx := component.WithRequestInfoFromHTTP(context.Background(), req)
	if err := htmsig.VerifyRequest(ctx, req.Header, resolver, htmsig.WithValidateExpires(true), htmsig.WithClock(fixedClock{now: now})); err != nil {
		return Claims{}, ErrInvalid
	}
	return Claims{KeyID: definition.KeyID(), Nonce: nonce, Expires: time.Unix(expires, 0).UTC()}, nil
}

// NewNonce returns a fresh 256-bit value suitable for the RFC 9421 nonce parameter.
func NewNonce() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate signature nonce: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validContentDigest(value string, body []byte) bool {
	digest := sha256.Sum256(body)
	want := "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
	return len(value) == len(want) && subtle.ConstantTimeCompare([]byte(value), []byte(want)) == 1
}

func hasRequiredComponents(components []component.Identifier, requireIdempotency bool) bool {
	want := map[string]bool{
		"@method": false, "@authority": false, "@target-uri": false, "@path": false,
		"authorization": false, "content-digest": false,
	}
	if requireIdempotency {
		want["idempotency-key"] = false
	}
	for _, id := range components {
		if len(id.Parameters()) != 0 {
			return false
		}
		if _, required := want[id.Name()]; required {
			want[id.Name()] = true
		}
	}
	for _, covered := range want {
		if !covered {
			return false
		}
	}
	return true
}

func readAndRestoreBody(req *http.Request, limit int64) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	closeErr := req.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func singleHeader(header http.Header, name string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] != ""
}

func validKeyID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validNonce(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

type singleKeyResolver struct {
	keyID string
	key   []byte
}

func (r singleKeyResolver) ResolveKey(keyID string) (any, error) {
	if keyID != r.keyID {
		return nil, ErrInvalid
	}
	return r.key, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }
