package bridge

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"testing"
)

// malformedCBCForTest returns a CBC ciphertext whose final padding byte is
// valid in range but whose preceding padding byte is not. It exercises the
// decrypt-side padding validation without relying on random ciphertext.
func malformedCBCForTest(key []byte) string {
	plain := make([]byte, aes.BlockSize*2)
	plain[len(plain)-1] = 2
	b, _ := aes.NewCipher(key)
	raw := make([]byte, len(plain))
	cipher.NewCBCEncrypter(b, key[:aes.BlockSize]).CryptBlocks(raw, plain)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestRootCryptoFallbackAndMalformedPadding(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	c := WeComCrypto{AESKey: key, Receiver: "corp"}
	// A nil Rand intentionally uses crypto/rand.Reader.
	enc, err := c.Encrypt([]byte("fallback"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Decrypt(enc); err != nil || string(got) != "fallback" {
		t.Fatalf("fallback round trip got %q, %v", got, err)
	}
	if _, err := (WeComCrypto{}).Decrypt(enc); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := c.Decrypt(malformedCBCForTest(key)); err == nil {
		t.Fatal("malformed padding accepted")
	}
}

func TestRootSendAndCallbackValidationGaps(t *testing.T) {
	s := testServer(MemoryAdapter{})
	tok := tokenFor(t, s)
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok,
		`{"touser":"bad id","msgtype":"text","agentid":1002,"text":{"content":"x"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid touser status %d", rr.Code)
	}
	s.cfg.MaxChunkBytes = 1
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok,
		`{"touser":"u1","msgtype":"text","agentid":1002,"text":{"content":"é"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid chunk status %d", rr.Code)
	}

	// A valid envelope with a bad signature must be rejected after trying all
	// configured bindings.
	s.cfg.MaxChunkBytes = DefaultChunkBytes
	env, err := s.BuildCallback("b1", InboundMessage{FromUserName: "u1", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	badPath := "/wecom/callback?timestamp=" + env.Timestamp + "&nonce=" + env.Nonce + "&msg_signature=bad"
	if rr := req(s, http.MethodPost, badPath, string(env.Body)); rr.Code != http.StatusForbidden {
		t.Fatalf("bad callback signature status %d", rr.Code)
	}

	// A correctly signed envelope whose ciphertext has invalid padding must
	// also be rejected rather than passed to the inbound callback.
	b := testBinding()
	c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID}
	key, err := DecodeAESKey(b.CallbackAESKey)
	if err != nil {
		t.Fatal(err)
	}
	enc := malformedCBCForTest(key)
	body, err := marshalEnvelope(CallbackEnvelope{Encrypt: enc})
	if err != nil {
		t.Fatal(err)
	}
	path := "/wecom/callback?timestamp=1&nonce=n&msg_signature=" + c.Signature("1", "n", enc)
	if rr := req(s, http.MethodPost, path, string(body)); rr.Code != http.StatusForbidden {
		t.Fatalf("malformed callback status %d", rr.Code)
	}
}

func TestRootUTF8AndAgentNumericValidationGaps(t *testing.T) {
	if _, err := ChunkUTF8(string([]byte{0xff}), 8); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := parseAgentID([]byte("-1")); err == nil {
		t.Fatal("negative numeric agent id accepted")
	}
}
