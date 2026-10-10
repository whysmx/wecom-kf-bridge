package wecom

import (
	"context"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@qyapi.weixin.qq.com/cgi-bin/x?access_token=SECRET#frag": "https://qyapi.weixin.qq.com/cgi-bin/x",
		"https://qyapi.weixin.qq.com?":                                       "https://qyapi.weixin.qq.com",
		"://bad":                                                             "[invalid url]",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

// #42: transport errors must not carry access_token / secrets.
func TestTransportErrorIsRedacted(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "corp", "CORPSECRETVALUE")
	c.MaxRetries = 0
	_, err := c.GetUser(context.Background(), "ACCESSTOKENVALUE", "u")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "ACCESSTOKENVALUE") || strings.Contains(err.Error(), "access_token") {
		t.Fatal(err)
	}
	_, err = c.GetToken(context.Background())
	if err == nil || strings.Contains(err.Error(), "CORPSECRETVALUE") {
		t.Fatal(err)
	}
}
