package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var ErrInvalidAESKey = errors.New("wecom: invalid AES key")

func DecodeAESKey(encoded string) ([]byte, error) {
	s := strings.TrimSpace(encoded)
	if s == "" {
		return nil, ErrInvalidAESKey
	}
	for len(s)%4 != 0 {
		s += "="
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, ErrInvalidAESKey
	}
	return b, nil
}
func Signature(token, timestamp, nonce, encrypted string) string {
	v := []string{token, timestamp, nonce, encrypted}
	sort.Strings(v)
	h := sha1.Sum([]byte(strings.Join(v, "")))
	return hex.EncodeToString(h[:])
}
func VerifySignature(token, timestamp, nonce, encrypted, sig string) bool {
	return strings.EqualFold(Signature(token, timestamp, nonce, encrypted), strings.TrimSpace(sig))
}

func Encrypt(key []byte, message, receiver string) (string, error) {
	if len(key) != 32 {
		return "", ErrInvalidAESKey
	}
	if !utf8Valid(message) {
		return "", fmt.Errorf("wecom: message is not UTF-8")
	}
	plain := make([]byte, 16+4+len(message)+len(receiver))
	if _, err := io.ReadFull(rand.Reader, plain[:16]); err != nil {
		return "", err
	}
	binary.BigEndian.PutUint32(plain[16:20], uint32(len(message)))
	copy(plain[20:], message)
	copy(plain[20+len(message):], receiver)
	padded := pkcs7Pad32(plain)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}
func Decrypt(key []byte, encoded, receiver string) (string, error) {
	if len(key) != 32 {
		return "", ErrInvalidAESKey
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", fmt.Errorf("wecom: invalid ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, raw)
	plain, err = pkcs7Unpad32(plain)
	if err != nil {
		return "", err
	}
	if len(plain) < 20 {
		return "", fmt.Errorf("wecom: plaintext too short")
	}
	n := int(binary.BigEndian.Uint32(plain[16:20]))
	if n < 0 || 20+n > len(plain) {
		return "", fmt.Errorf("wecom: invalid message length")
	}
	msg := plain[20 : 20+n]
	got := plain[20+n:]
	if receiver != "" && !bytes.Equal(got, []byte(receiver)) {
		return "", fmt.Errorf("wecom: receiver mismatch")
	}
	if !utf8Valid(string(msg)) {
		return "", fmt.Errorf("wecom: plaintext is not UTF-8")
	}
	return string(msg), nil
}
func pkcs7Pad32(in []byte) []byte {
	n := 32 - len(in)%32
	if n == 0 {
		n = 32
	}
	return append(append([]byte(nil), in...), bytes.Repeat([]byte{byte(n)}, n)...)
}
func pkcs7Unpad32(in []byte) ([]byte, error) {
	if len(in) == 0 || len(in)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("wecom: invalid padding")
	}
	n := int(in[len(in)-1])
	if n < 1 || n > 32 || n > len(in) {
		return nil, fmt.Errorf("wecom: invalid padding")
	}
	for _, v := range in[len(in)-n:] {
		if int(v) != n {
			return nil, fmt.Errorf("wecom: invalid padding")
		}
	}
	return in[:len(in)-n], nil
}
func utf8Valid(s string) bool {
	for i := 0; i < len(s); {
		b := s[i]
		n := 1
		switch {
		case b < 0x80:
			n = 1
		case b >= 0xc2 && b <= 0xdf:
			n = 2
		case b >= 0xe0 && b <= 0xef:
			n = 3
		case b >= 0xf0 && b <= 0xf4:
			n = 4
		default:
			return false
		}
		if i+n > len(s) {
			return false
		}
		for j := 1; j < n; j++ {
			if s[i+j]&0xc0 != 0x80 {
				return false
			}
		}
		i += n
	}
	return true
}
