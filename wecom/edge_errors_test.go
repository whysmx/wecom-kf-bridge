package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientStopsWhenContextEndsDuringLimitOrBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "s")
	c.RatePerSecond, c.Burst, c.MaxRetries = 0.001, 1, 0
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = c.GetToken(ctx) // consumes the only token
	cancel()
	if _, err := c.GetToken(ctx); err == nil {
		t.Fatal("rate limiter ignored cancelled context")
	}
	// cancelled while backing off after a retryable 503: no further attempt
	calls := 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer srv2.Close()
	c2 := NewClient(srv2.URL, "c", "s")
	c2.MaxRetries = 5
	c2.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := c2.GetToken(context.Background()); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func cbcEncrypt(t *testing.T, key, plain []byte) string {
	t.Helper()
	b, _ := aes.NewCipher(key)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(b, key[:16]).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out)
}

func TestDecryptRejectsMalformedPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	frame := func(n uint32, msg, recv string) []byte {
		p := make([]byte, 20)
		binary.BigEndian.PutUint32(p[16:], n)
		return pkcs7Pad32(append(append(p, msg...), recv...))
	}
	cases := map[string][]byte{
		"bad padding": bytes.Repeat([]byte{0}, 32),
		"too short":   pkcs7Pad32([]byte("short")),
		"bad length":  frame(1000, "hi", "corp"),
		"not utf8":    frame(2, "\xff\xfe", "corp"),
	}
	for name, p := range cases {
		if _, err := Decrypt(key, cbcEncrypt(t, key, p), "corp"); !errors.Is(err, ErrCiphertext) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Decrypt(key[:16], "x", "corp"); !errors.Is(err, ErrInvalidAESKey) {
		t.Fatal("short key")
	}
	if got := pkcs7Pad32(make([]byte, 32)); len(got) != 64 {
		t.Fatal("full block padding")
	}
}
