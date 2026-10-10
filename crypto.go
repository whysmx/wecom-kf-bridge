package bridge

import (
	"errors"
	"fmt"
	"io"

	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// The root package no longer carries its own AES/signature code: WeComCrypto
// is a thin adapter over the single implementation in package wecom so both
// callback directions share identical padding, receiver and signature rules.
var (
	ErrSignature = errors.New("bridge: invalid signature")
	ErrCrypto    = wecom.ErrCiphertext
	ErrReceiver  = wecom.ErrReceiver
)

// WeComCrypto implements the AES-256-CBC callback format. AESKey accepts the
// usual 43-character base64 key, or a raw 32-byte []byte for test vectors.
type WeComCrypto struct {
	Token    string
	AESKey   any
	CorpID   string
	Receiver string // alias accepted by callers that name the receive ID
	Rand     io.Reader
}

func (c WeComCrypto) receiver() string {
	if c.CorpID != "" {
		return c.CorpID
	}
	return c.Receiver
}

func DecodeAESKey(encoded string) ([]byte, error) {
	b, err := wecom.DecodeAESKey(encoded)
	if err != nil {
		return nil, fmt.Errorf("bridge: AES key must decode to 32 bytes: %w", err)
	}
	return b, nil
}
func (c WeComCrypto) key() ([]byte, error) {
	switch v := c.AESKey.(type) {
	case string:
		return DecodeAESKey(v)
	case []byte:
		if len(v) != 32 {
			return nil, fmt.Errorf("bridge: AES key must be 32 bytes")
		}
		return append([]byte(nil), v...), nil
	case nil:
		return nil, fmt.Errorf("bridge: missing AES key")
	default:
		return nil, fmt.Errorf("bridge: unsupported AES key")
	}
}
func (c WeComCrypto) Encrypt(plain []byte) (string, error) {
	k, e := c.key()
	if e != nil {
		return "", e
	}
	return wecom.EncryptWithRand(k, string(plain), c.receiver(), c.Rand)
}
func (c WeComCrypto) Decrypt(encoded string) ([]byte, error) {
	k, e := c.key()
	if e != nil {
		return nil, e
	}
	p, e := wecom.Decrypt(k, encoded, c.receiver())
	if e != nil {
		return nil, e
	}
	return []byte(p), nil
}
func Signature(token, timestamp, nonce, encrypted string) string {
	return wecom.Signature(token, timestamp, nonce, encrypted)
}
func (c WeComCrypto) Signature(timestamp, nonce, encrypted string) string {
	return Signature(c.Token, timestamp, nonce, encrypted)
}
func VerifySignature(token, timestamp, nonce, encrypted, signature string) bool {
	return wecom.VerifySignature(token, timestamp, nonce, encrypted, signature)
}
func (c WeComCrypto) VerifySignature(signature, timestamp, nonce, encrypted string) bool {
	return VerifySignature(c.Token, timestamp, nonce, encrypted, signature)
}
