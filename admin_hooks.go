package bridge

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// ApplyBinding installs or replaces a virtual binding at runtime (admin
// enable/disable/rebind/rotate). Every token issued for the binding is
// revoked, so requests authorised under the old revision or credentials
// cannot continue.
func (s *Server) ApplyBinding(b Binding) error {
	if b.ID == "" || b.CorpID == "" {
		return errors.New("bridge: binding id and corp_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.byCorp[b.CorpID]; ok && owner != b.ID {
		return fmt.Errorf("bridge: duplicate virtual corp_id %q", b.CorpID)
	}
	if old, ok := s.bindings[b.ID]; ok {
		delete(s.byCorp, old.CorpID)
	}
	s.bindings[b.ID] = b
	s.byCorp[b.CorpID] = b.ID
	for k, t := range s.tokens {
		if t.BindingID == b.ID {
			delete(s.tokens, k)
		}
	}
	return nil
}

// VerifyChallenge builds the WeCom URL-verification request cc-connect
// answers (GET with an encrypted echostr) and the plaintext it must echo.
func (s *Server) VerifyChallenge(bindingID string) (url.Values, string, error) {
	b, ok := s.binding(bindingID)
	if !ok {
		return nil, "", errors.New("bridge: binding not found")
	}
	nonce, err := s.randomNonce()
	if err != nil {
		return nil, "", err
	}
	echo := "verify-" + nonce
	c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID, Rand: s.cfg.Random}
	enc, err := c.Encrypt([]byte(echo))
	if err != nil {
		return nil, "", err
	}
	ts := strconv.FormatInt(s.cfg.Clock().Unix(), 10)
	return url.Values{"msg_signature": {c.Signature(ts, nonce, enc)}, "timestamp": {ts}, "nonce": {nonce}, "echostr": {enc}}, echo, nil
}
