package bridge

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	ErrSignature = errors.New("bridge: invalid signature")
	ErrCrypto    = errors.New("bridge: invalid encrypted payload")
	ErrReceiver  = errors.New("bridge: encrypted payload receiver mismatch")
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
	s := strings.TrimSpace(encoded)
	if s == "" {
		return nil, fmt.Errorf("bridge: empty AES key")
	}
	// WeCom supplies base64 without the final '='. RawStdEncoding handles
	// that form; padded input is accepted for interoperability.
	var b []byte
	var err error
	if strings.Contains(s, "=") {
		b, err = base64.StdEncoding.DecodeString(s)
	} else {
		b, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil && len(s)%4 != 0 {
			b, err = base64.StdEncoding.DecodeString(s + strings.Repeat("=", (4-len(s)%4)%4))
		}
	}
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("bridge: AES key must decode to 32 bytes")
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
func (c WeComCrypto) block() (cipher.Block, error) {
	k, e := c.key()
	if e != nil {
		return nil, e
	}
	return aes.NewCipher(k)
}
func (c WeComCrypto) Encrypt(plain []byte) (string, error) {
	k, e := c.key()
	if e != nil {
		return "", e
	}
	if !utf8.Valid(plain) {
		return "", fmt.Errorf("bridge: plaintext is not UTF-8")
	}
	if len(plain) > int(^uint32(0)) {
		return "", fmt.Errorf("bridge: plaintext too large")
	}
	rnd := c.Rand
	if rnd == nil {
		rnd = crand.Reader
	}
	prefix := make([]byte, 16)
	if _, e = io.ReadFull(rnd, prefix); e != nil {
		return "", e
	}
	msg := make([]byte, 0, 20+len(plain)+len(c.receiver()))
	msg = append(msg, prefix...)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(plain)))
	msg = append(msg, l[:]...)
	msg = append(msg, plain...)
	msg = append(msg, []byte(c.receiver())...)
	msg = pkcs7(msg, aes.BlockSize*2)
	b, _ := aes.NewCipher(k)
	out := make([]byte, len(msg))
	cipher.NewCBCEncrypter(b, k[:aes.BlockSize]).CryptBlocks(out, msg)
	return base64.StdEncoding.EncodeToString(out), nil
}
func (c WeComCrypto) Decrypt(encoded string) ([]byte, error) {
	k, e := c.key()
	if e != nil {
		return nil, e
	}
	raw, e := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if e != nil || len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return nil, ErrCrypto
	}
	b, _ := aes.NewCipher(k)
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(b, k[:aes.BlockSize]).CryptBlocks(plain, raw)
	plain, e = unpkcs7(plain, aes.BlockSize*2)
	if e != nil {
		return nil, e
	}
	if len(plain) < 20 {
		return nil, ErrCrypto
	}
	n := int(binary.BigEndian.Uint32(plain[16:20]))
	if n < 0 || n > len(plain)-20 {
		return nil, ErrCrypto
	}
	end := 20 + n
	if c.receiver() != "" && !bytes.Equal(plain[end:], []byte(c.receiver())) {
		return nil, ErrReceiver
	}
	return append([]byte(nil), plain[20:end]...), nil
}
func pkcs7(v []byte, block int) []byte {
	n := block - len(v)%block
	if n == 0 {
		n = block
	}
	return append(v, bytes.Repeat([]byte{byte(n)}, n)...)
}
func unpkcs7(v []byte, block int) ([]byte, error) {
	if len(v) == 0 || len(v)%block != 0 {
		return nil, ErrCrypto
	}
	n := int(v[len(v)-1])
	if n < 1 || n > block || n > len(v) {
		return nil, ErrCrypto
	}
	for _, x := range v[len(v)-n:] {
		if int(x) != n {
			return nil, ErrCrypto
		}
	}
	return v[:len(v)-n], nil
}
func Signature(token, timestamp, nonce, encrypted string) string {
	a := []string{token, timestamp, nonce, encrypted}
	sort.Strings(a)
	h := sha1.Sum([]byte(strings.Join(a, "")))
	return fmt.Sprintf("%x", h[:])
}
func (c WeComCrypto) Signature(timestamp, nonce, encrypted string) string {
	return Signature(c.Token, timestamp, nonce, encrypted)
}
func VerifySignature(token, timestamp, nonce, encrypted, signature string) bool {
	want := Signature(token, timestamp, nonce, encrypted)
	if len(want) != len(signature) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(signature))) == 1
}
func (c WeComCrypto) VerifySignature(signature, timestamp, nonce, encrypted string) bool {
	return VerifySignature(c.Token, timestamp, nonce, encrypted, signature)
}
