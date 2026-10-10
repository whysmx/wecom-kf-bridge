package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

// ErrNoSealer is returned when sensitive content (customer message bodies,
// pull tokens) would be persisted without an at-rest encryption key.
var ErrNoSealer = errors.New("state: no at-rest encryption key configured")

// ErrSealed reports a stored value that cannot be decrypted with the current key.
var ErrSealed = errors.New("state: sealed value cannot be opened")

const sealPrefix = "enc:v1:"

// Sealer encrypts sensitive columns with AES-256-GCM. The master key comes
// from outside the database (docs/09 §4: master_key_env) and is never stored.
type Sealer struct{ aead cipher.AEAD }

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("state: master key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: g}, nil
}

// ParseMasterKey accepts a base64 (std or raw, url or std alphabet) 32-byte key.
func ParseMasterKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("state: master key must be base64 of 32 bytes")
}

func (s *Sealer) seal(plain, aad string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return sealPrefix + base64.RawStdEncoding.EncodeToString(out), nil
}
func (s *Sealer) open(v, aad string) (string, error) {
	if !strings.HasPrefix(v, sealPrefix) {
		return "", ErrSealed
	}
	raw, err := base64.RawStdEncoding.DecodeString(v[len(sealPrefix):])
	n := s.aead.NonceSize()
	if err != nil || len(raw) < n {
		return "", ErrSealed
	}
	p, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", ErrSealed
	}
	return string(p), nil
}

// sealField encrypts a sensitive value; empty stays empty. aad binds the
// ciphertext to its column so values cannot be swapped between columns.
func (st *Store) sealField(v, aad string) (string, error) {
	if v == "" {
		return "", nil
	}
	if st.sealer == nil {
		return "", ErrNoSealer
	}
	return st.sealer.seal(v, aad)
}
func (st *Store) openField(v, aad string) (string, error) {
	if v == "" {
		return "", nil
	}
	if st.sealer == nil {
		return "", ErrNoSealer
	}
	return st.sealer.open(v, aad)
}
