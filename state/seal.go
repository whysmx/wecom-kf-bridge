package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"
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

// SaveSyncToken stores the callback pull token sealed, with its expiry, and
// marks the scope pending in the same statement (docs/04 §3).
func (s *Store) SaveSyncToken(ctx context.Context, scopeID, token string, expiresAt time.Time) error {
	if scopeID == "" {
		return ErrInvalidID
	}
	sealed, err := s.sealField(token, "sync.token:"+scopeID)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE sync_scopes SET sync_token=?,sync_token_expires_at=?,pending=1,updated_at=? WHERE id=?`, sealed, unix(expiresAt), unix(s.now()), scopeID)
	if err != nil {
		return err
	}
	return rowsOrNotFound(r)
}

// SyncToken returns the unexpired pull token for a scope, or "".
func (s *Store) SyncToken(ctx context.Context, scopeID string) (string, error) {
	var v string
	var exp int64
	if err := s.db.QueryRowContext(ctx, `SELECT sync_token,sync_token_expires_at FROM sync_scopes WHERE id=?`, scopeID).Scan(&v, &exp); err != nil {
		return "", mapNotFound(err)
	}
	if v == "" || (exp != 0 && !s.now().Before(timeFrom(exp))) {
		return "", nil
	}
	return s.openField(v, "sync.token:"+scopeID)
}
