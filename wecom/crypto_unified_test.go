package wecom

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDecryptRejectsEmptyReceiver(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	enc, err := Encrypt(key, "<xml/>", "corp-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(key, enc, ""); !errors.Is(err, ErrReceiver) {
		t.Fatalf("empty receiver must be rejected, got %v", err)
	}
	if _, err := Decrypt(key, enc, "corp-b"); !errors.Is(err, ErrReceiver) {
		t.Fatalf("wrong receiver accepted: %v", err)
	}
	if _, err := Encrypt(key, "x", ""); !errors.Is(err, ErrReceiver) {
		t.Fatalf("encrypt without receiver: %v", err)
	}
	if p, err := Decrypt(key, enc, "corp-a"); err != nil || p != "<xml/>" {
		t.Fatalf("round trip %q %v", p, err)
	}
	if _, err := Decrypt(key, "!!", "corp-a"); !errors.Is(err, ErrCiphertext) {
		t.Fatalf("ciphertext error class: %v", err)
	}
}

func TestVerifySignatureConstantTimeSemantics(t *testing.T) {
	sig := Signature("tok", "1", "n", "enc")
	if !VerifySignature("tok", "1", "n", "enc", sig) || !VerifySignature("tok", "1", "n", "enc", strings.ToUpper(sig)) {
		t.Fatal("valid signature rejected")
	}
	for _, bad := range []string{"", sig[:39], sig + "0", strings.Repeat("0", 40)} {
		if VerifySignature("tok", "1", "n", "enc", bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestWebhookRequiresReceiver(t *testing.T) {
	if _, err := NewWebhook("tok", strings.TrimRight("BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=", "="), ""); err == nil {
		t.Fatal("webhook without receiver accepted")
	}
}
