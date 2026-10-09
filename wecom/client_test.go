package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChunkTextRuneBoundary(t *testing.T) {
	got, err := ChunkText("😀界abc", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "😀" || got[1] != "界ab" || got[2] != "c" {
		t.Fatalf("chunks %#v", got)
	}
}
func TestClientPathsAndCustomer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"tok","expires_in":3600}`))
		case "/cgi-bin/kf/customer/batchget":
			var p map[string]any
			if json.NewDecoder(r.Body).Decode(&p) != nil {
				t.Fail()
			}
			if r.Method != "POST" || len(p["external_userid_list"].([]any)) != 1 {
				t.Fatalf("request %#v", p)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"customer_list":[{"external_userid":"u","nickname":"N"}]}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "corp", "secret")
	tok, err := c.GetToken(context.Background())
	if err != nil || tok.AccessToken != "tok" {
		t.Fatalf("token %#v %v", tok, err)
	}
	resp, err := c.CustomerBatchGet(context.Background(), tok.AccessToken, CustomerBatchGetRequest{ExternalUserIDs: []string{"u"}})
	if err != nil || len(resp.Customers) != 1 || resp.Customers[0].Nickname != "N" {
		t.Fatalf("customer %#v %v", resp, err)
	}
}
func TestCryptoReceiverAndSignature(t *testing.T) {
	key := strings.Repeat("A", 43)
	w, err := NewWebhook("token", key, "corp")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := Encrypt(w.AESKey, "<xml>😀</xml>", "corp")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifySignature("token", "1", "n", enc, Signature("token", "1", "n", enc)) {
		t.Fatal("signature")
	}
	if _, err := Decrypt(w.AESKey, enc, "other"); err == nil {
		t.Fatal("receiver accepted")
	}
}
