package bridge

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChunkAndIDHelpers(t *testing.T) {
	if _, err := SplitUTF8("😀", 1); err == nil {
		t.Fatal("split")
	}
	if c, err := ChunkUTF8("", 1); err != nil || len(c) != 0 {
		t.Fatal(err)
	}
	if c, _ := ChunkUTF8("ab界", 0); len(c) != 1 {
		t.Fatal("default size")
	}
	if c, _ := ChunkUTF8("a界b", 3); strings.Join(c, "|") != "a|界|b" {
		t.Fatal(c)
	}
	if _, err := ChunkUTF8("\xff", 3); err == nil {
		t.Fatal("invalid utf8")
	}
	if !validAgentID("1") || validAgentID("-1") || validAgentID("1.2") || validAgentID("") || validAgentID("18446744073709551616") {
		t.Fatal("agent")
	}
	if !sameAgentID("01", "1") || sameAgentID("x", "1") {
		t.Fatal("same")
	}
	for raw, ok := range map[string]bool{`"1002"`: true, `1002`: true, `"`: false, ``: false, `1.0`: false, `1e3`: false, `"x"`: false, `-1`: false, `true`: false} {
		if _, err := parseAgentID([]byte(raw)); (err == nil) != ok {
			t.Fatalf("%s", raw)
		}
	}
	var got string
	LoggerFunc(func(e string, _ map[string]any) { got = e }).Log("event", nil)
	(LoggerFunc(nil)).Log("x", nil)
	(nopLogger{}).Log("x", nil)
	if got != "event" {
		t.Fatal(got)
	}
	if _, err := unmarshalInbound([]byte("<bad")); err == nil {
		t.Fatal("xml")
	}
	if _, err := unmarshalEnvelope([]byte("<xml></xml>")); err == nil {
		t.Fatal("missing encrypt")
	}
	if _, err := unmarshalEnvelope([]byte("<bad")); err == nil {
		t.Fatal("bad envelope")
	}
	rr := httptest.NewRecorder()
	writeAPIError(rr, errors.New("x"))
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "70007") {
		t.Fatal(rr.Body.String())
	}
	rr = httptest.NewRecorder()
	writeAPIError(rr, errAPI{Code: 1})
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
}

func TestRootCryptoKeyForms(t *testing.T) {
	for _, raw := range []string{"", "bad", strings.Repeat("A", 44)} {
		if _, err := DecodeAESKey(raw); err == nil {
			t.Fatal(raw)
		}
	}
	for _, c := range []WeComCrypto{{AESKey: 123, CorpID: "c"}, {CorpID: "c"}, {AESKey: []byte{1}, CorpID: "c"}} {
		if _, err := c.Encrypt([]byte("x")); err == nil {
			t.Fatal("bad key accepted")
		}
		if _, err := c.Decrypt("x"); err == nil {
			t.Fatal("bad key accepted")
		}
	}
	c := WeComCrypto{AESKey: testBinding().CallbackAESKey, Receiver: "r"}
	if c.receiver() != "r" {
		t.Fatal("receiver alias")
	}
	if _, err := c.Encrypt([]byte{0xff}); err == nil {
		t.Fatal("utf8")
	}
	if _, err := c.Decrypt("bad"); err == nil {
		t.Fatal("decrypt")
	}
}
