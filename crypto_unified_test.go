package bridge

import (
	"bytes"
	"errors"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// The root adapter and package wecom must produce interchangeable payloads.
func TestRootCryptoDelegatesToWecom(t *testing.T) {
	k := bytes.Repeat([]byte{6}, 32)
	c := WeComCrypto{Token: "t", AESKey: k, CorpID: "corp"}
	enc, err := c.Encrypt([]byte("<xml>x</xml>"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := wecom.Decrypt(k, enc, "corp"); err != nil || p != "<xml>x</xml>" {
		t.Fatalf("wecom cannot read root ciphertext: %q %v", p, err)
	}
	enc2, _ := wecom.Encrypt(k, "y", "corp")
	if p, err := c.Decrypt(enc2); err != nil || string(p) != "y" {
		t.Fatalf("root cannot read wecom ciphertext: %v", err)
	}
	if c.Signature("1", "n", enc) != wecom.Signature("t", "1", "n", enc) {
		t.Fatal("signature mismatch")
	}
	empty := WeComCrypto{AESKey: k}
	if _, err := empty.Decrypt(enc); !errors.Is(err, ErrReceiver) {
		t.Fatalf("empty receiver accepted: %v", err)
	}
	if _, err := empty.Encrypt([]byte("x")); !errors.Is(err, ErrReceiver) {
		t.Fatalf("encrypt without receiver: %v", err)
	}
}
