package bridge

import (
	"net/http"
	"testing"
)

func TestApplyBindingRevokesTokensAndReplacesCredentials(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	b := testBinding()
	b.CorpSecret, b.Revision = "secret-2", 2
	if err := e.srv.ApplyBinding(b); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid="+e.cust.UID, ""); code == 200 {
		t.Fatal("old token still valid")
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", ""); code == 200 {
		t.Fatal("old secret accepted")
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-2", ""); code != 200 {
		t.Fatal("new secret refused")
	}
	b.CorpID = "corp-2"
	must(t, e.srv.ApplyBinding(b))
	if code, _ := e.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-2", ""); code == 200 {
		t.Fatal("old corp id still routed")
	}
	other := testBinding()
	other.ID, other.CorpID = "b2", "corp-2"
	if e.srv.ApplyBinding(other) == nil {
		t.Fatal("duplicate corp accepted")
	}
	if e.srv.ApplyBinding(Binding{}) == nil {
		t.Fatal("empty binding")
	}
}

func TestVerifyChallenge(t *testing.T) {
	e := newEnv(t)
	q, echo, err := e.srv.VerifyChallenge("b1")
	must(t, err)
	b := testBinding()
	c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID}
	if c.Signature(q.Get("timestamp"), q.Get("nonce"), q.Get("echostr")) != q.Get("msg_signature") {
		t.Fatal("signature")
	}
	plain, err := c.Decrypt(q.Get("echostr"))
	if err != nil || string(plain) != echo {
		t.Fatalf("%q %v", plain, err)
	}
	if _, _, err := e.srv.VerifyChallenge("nope"); err == nil {
		t.Fatal("unknown binding")
	}
	f := newEnv(t, func(c *Config) { c.Random = failReader{} })
	if _, _, err := f.srv.VerifyChallenge("b1"); err == nil {
		t.Fatal("no randomness")
	}
	bad := testBinding()
	bad.ID, bad.CorpID, bad.CallbackAESKey = "b3", "corp-3", "short"
	must(t, e.srv.ApplyBinding(bad))
	if _, _, err := e.srv.VerifyChallenge("b3"); err == nil {
		t.Fatal("bad key")
	}
}
