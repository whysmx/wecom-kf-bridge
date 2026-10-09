package bridge

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func encryptRawForTest(k, plain []byte) string {
	p := pkcs7(plain, aes.BlockSize*2)
	b, _ := aes.NewCipher(k)
	out := make([]byte, len(p))
	cipher.NewCBCEncrypter(b, k[:aes.BlockSize]).CryptBlocks(out, p)
	return base64.StdEncoding.EncodeToString(out)
}

func TestRootHelpersAndAdapters(t *testing.T) {
	a := MemoryAdapter{Users: map[string]Customer{"u": {UserID: "u", Name: "N"}}}
	if c, _ := a.GetUser(context.Background(), Binding{}, "u"); c.Name != "N" {
		t.Fatal(c)
	}
	if c, _ := a.GetUser(context.Background(), Binding{}, "x"); c.UserID != "x" {
		t.Fatal(c)
	}
	if err := a.SendText(context.Background(), SendRequest{}); err != nil {
		t.Fatal(err)
	}
	var got string
	LoggerFunc(func(e string, _ map[string]any) { got = e }).Log("event", nil)
	if got != "event" {
		t.Fatal(got)
	}
	(LoggerFunc(nil)).Log("x", nil)
	(nopLogger{}).Log("x", nil)
	if _, err := SplitUTF8("😀", 1); err == nil {
		t.Fatal("split")
	}
	if _, err := ChunkUTF8("", 1); err != nil {
		t.Fatal(err)
	}
	if !validAgentID("1") || validAgentID("-1") || validAgentID("1.2") {
		t.Fatal("agent")
	}
	if sameAgentID("01", "1") != true || sameAgentID("x", "1") {
		t.Fatal("same")
	}
	if validAgentID("18446744073709551616") {
		t.Fatal("overflow")
	}
	if _, err := parseAgentID([]byte(`"`)); err == nil {
		t.Fatal("quote")
	}
	for _, raw := range []string{"", "bad", strings.Repeat("A", 44)} {
		if _, err := DecodeAESKey(raw); err == nil {
			t.Fatal(raw)
		}
	}
	k := bytes.Repeat([]byte{1}, 32)
	if _, err := DecodeAESKey(base64.StdEncoding.EncodeToString(k)); err != nil {
		t.Fatal(err)
	}
	cs := WeComCrypto{AESKey: k, Receiver: "r", Rand: bytes.NewReader(bytes.Repeat([]byte{2}, 64))}
	if cs.receiver() != "r" {
		t.Fatal()
	}
	if _, err := cs.block(); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Encrypt([]byte{0xff}); err == nil {
		t.Fatal("utf8")
	}
	if _, err := (WeComCrypto{AESKey: k, Rand: badReader{}}).Encrypt([]byte("x")); err == nil {
		t.Fatal("rand")
	}
	if len(pkcs7(make([]byte, 16), 16)) != 32 {
		t.Fatal("pad full")
	}
	if _, err := (WeComCrypto{AESKey: 123}).block(); err == nil {
		t.Fatal("type")
	}
	if _, err := (WeComCrypto{}).block(); err == nil {
		t.Fatal("nil key")
	}
	if _, err := (WeComCrypto{AESKey: []byte{1}}).block(); err == nil {
		t.Fatal("short")
	}
	if _, err := unpkcs7(nil, 16); err == nil {
		t.Fatal("pad")
	}
	if _, err := unpkcs7(make([]byte, 16), 16); err == nil {
		t.Fatal("pad2")
	}
	if len(pkcs7([]byte("x"), 16)) != 16 {
		t.Fatal("pkcs")
	}
	if _, err := cs.Decrypt("bad"); err == nil {
		t.Fatal("decrypt")
	}
	if _, err := cs.Decrypt(encryptRawForTest(k, make([]byte, 0))); err == nil {
		t.Fatal("short plain")
	}
	raw := make([]byte, 20)
	binary.BigEndian.PutUint32(raw[16:20], 999)
	if _, err := cs.Decrypt(encryptRawForTest(k, raw)); err == nil {
		t.Fatal("length")
	}
	if _, err := ChunkUTF8("é", 1); err == nil {
		t.Fatal("boundary")
	}
	if _, err := unmarshalInbound([]byte("<bad")); err == nil {
		t.Fatal("xml")
	}
	if _, err := unmarshalEnvelope([]byte("<xml></xml>")); err == nil {
		t.Fatal("envelope")
	}
	if err := (errAPI{}).Error(); err != "" {
		t.Fatal(err)
	}
	s := NewServer(Config{Bindings: []Binding{testBinding()}, Customers: []Customer{{BindingID: "b1", UserID: "u", Authorized: false}}})
	s.mu.Lock()
	s.customers["b1\x00z"] = Customer{UserID: "z"}
	s.mu.Unlock()
	if c, ok := s.customer(testBinding(), "z"); !ok || c.BindingID != "b1" {
		t.Fatal(c, ok)
	}
	bb := testBinding()
	bb.CallbackAESKey = "bad"
	sb := NewServer(Config{Bindings: []Binding{bb}})
	if _, err := sb.BuildCallback("b1", InboundMessage{FromUserName: "u"}); err == nil {
		t.Fatal("bad callback key")
	}
	rr := httptest.NewRecorder()
	writeAPIError(rr, errors.New("x"))
	if rr.Code != 500 {
		t.Fatal(rr.Code)
	}
	rr = httptest.NewRecorder()
	writeAPIError(rr, errAPI{Code: 1})
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	_ = base64.StdEncoding
}
