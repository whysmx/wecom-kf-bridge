package wecom

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// errReader is used to exercise response/body I/O failures without relying on
// a real network failure occurring at a particular point in the request.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errReader) Close() error             { return nil }

type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: errReader{}, Header: make(http.Header)}, nil
}

func TestClientCoverageGapBranches(t *testing.T) {
	ctx := context.Background()
	c := NewClient("http://example.invalid", "corp", "secret")
	c.HTTPClient = &http.Client{Transport: errRoundTripper{}}
	if err := c.doJSON(ctx, http.MethodGet, "/x", "", nil, nil, nil); err == nil {
		t.Fatal("expected response read error")
	}
	if _, err := c.GetTokenFor(ctx, "corp", "secret"); err == nil {
		t.Fatal("expected token response read error")
	}

	// A valid envelope followed by a field type mismatch reaches doJSON's
	// output-unmarshal error path (checkEnvelope itself still succeeds).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/user/get":
			_, _ = io.WriteString(w, `{"errcode":0,"userid":123}`)
		case "/cgi-bin/gettoken":
			_, _ = io.WriteString(w, `{"errcode":9,"errmsg":"denied"}`)
		default:
			_, _ = io.WriteString(w, `{"errcode":0}`)
		}
	}))
	defer srv.Close()
	c = NewClient(srv.URL, "corp", "secret")
	if _, err := c.GetUser(ctx, "token", "u"); err == nil {
		t.Fatal("expected output unmarshal error")
	}
	if _, err := c.GetTokenFor(ctx, "corp", "secret"); err == nil {
		t.Fatal("expected API error")
	}
	if _, err := c.GetUser(ctx, "", "u"); err == nil {
		t.Fatal("expected token validation error")
	}
	if _, err := c.GetUser(ctx, "token", ""); err == nil {
		t.Fatal("expected user validation error")
	}
	if _, err := c.TransServiceState(ctx, "token", ServiceStateRequest{}); err == nil {
		t.Fatal("expected service identity validation error")
	}
	if _, err := c.SendTextChunked(ctx, "token", SendRequest{ToUser: "u", OpenKfID: "k", MsgType: "text"}, "\xff"); err == nil {
		t.Fatal("expected invalid UTF-8 error")
	}
}

func TestCryptoCoverageGapBranches(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	oldReader := cryptorand.Reader
	defer func() { cryptorand.Reader = oldReader }()
	cryptorand.Reader = errReader{}
	if _, err := Encrypt(key, "message", "receiver"); err == nil {
		t.Fatal("expected random source error")
	}

	if _, err := pkcs7Unpad32(append(make([]byte, 15), 2)); err == nil {
		t.Fatal("expected mismatched padding error")
	}

	// Build CBC ciphertexts for malformed, but correctly padded, plaintexts.
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(plain []byte) string {
		p := pkcs7Pad32(plain)
		out := make([]byte, len(p))
		cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, p)
		return base64.StdEncoding.EncodeToString(out)
	}
	if _, err := Decrypt(key, encode([]byte("short")), ""); err == nil {
		t.Fatal("expected plaintext-too-short error")
	}
	badLen := make([]byte, 20)
	binary.BigEndian.PutUint32(badLen[16:20], 100)
	if _, err := Decrypt(key, encode(badLen), ""); err == nil {
		t.Fatal("expected invalid message length error")
	}
	invalidUTF8 := make([]byte, 21)
	binary.BigEndian.PutUint32(invalidUTF8[16:20], 1)
	invalidUTF8[20] = 0xff
	if _, err := Decrypt(key, encode(invalidUTF8), ""); err == nil {
		t.Fatal("expected invalid plaintext UTF-8 error")
	}
}

type captureStateStore struct{ msgs []state.InboxMessage }

func (s *captureStateStore) CommitSyncPage(_ context.Context, _ string, _ string, _ bool, msgs []state.InboxMessage) (int, error) {
	s.msgs = append(s.msgs, msgs...)
	return len(msgs), nil
}

func TestSyncAllCoverageGapBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"next_cursor":"done","has_more":0,"msg_list":[{"msgid":"","msgtype":"text"}]}`)
	}))
	defer srv.Close()
	store := &captureStateStore{}
	res, err := NewClient(srv.URL, "c", "s").SyncAll(context.Background(), SyncOptions{Scope: "scope", AccessToken: "token", Store: store})
	if err != nil || res.Messages != 1 || len(store.msgs) != 1 {
		t.Fatalf("result=%#v msgs=%#v err=%v", res, store.msgs, err)
	}
	if store.msgs[0].BindingID != "scope" || store.msgs[0].ExternalMsgID == "" {
		t.Fatalf("fallback IDs not assigned: %#v", store.msgs[0])
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (failingBody) Close() error             { return nil }

func TestWebhookCoverageGapBranches(t *testing.T) {
	w, err := NewWebhook("token", validKeyString(), "corp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.VerifyEchostr(w.Signature("1", "n", "AAAA"), "1", "n", "AAAA"); err == nil {
		t.Fatal("expected decrypt error")
	}
	w.MaxBodyBytes = 0
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/?msg_signature=x&timestamp=1&nonce=n", nil))
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusBadRequest {
		t.Fatalf("unexpected zero-limit status %d", rr.Code)
	}
	w.MaxBodyBytes = 1024
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/?msg_signature=x&timestamp=1&nonce=n", nil)
	req.Body = failingBody{}
	w.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unexpected body-read status %d", rr.Code)
	}
}
