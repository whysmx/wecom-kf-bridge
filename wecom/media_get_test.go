package wecom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetMedia_SuccessAndHeaders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cgi-bin/media/get" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("access_token") == "" || r.URL.Query().Get("media_id") != "MID" {
			t.Fatalf("query %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
		_, _ = w.Write([]byte("%PDF-1.4"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "corp", "secret")
	c.HTTPClient = srv.Client()
	md, err := c.GetMedia(context.Background(), "tok", "MID", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(md.Data) != "%PDF-1.4" || md.ContentType != "application/pdf" || !strings.Contains(md.ContentDisposition, "report.pdf") {
		t.Fatalf("%+v", md)
	}
}

func TestGetMedia_JSONErrorAndOversize(t *testing.T) {
	t.Parallel()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode":40007,"errmsg":"invalid media_id"}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytesOf(20))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "corp", "secret")
	c.HTTPClient = srv.Client()
	_, err := c.GetMedia(context.Background(), "tok", "bad", 1024)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 40007 {
		t.Fatalf("err=%v", err)
	}
	_, err = c.GetMedia(context.Background(), "tok", "big", 10)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err=%v", err)
	}
}

func TestGetMedia_RequiresArgs(t *testing.T) {
	t.Parallel()
	c := NewClient("https://example.invalid", "c", "s")
	if _, err := c.GetMedia(context.Background(), "", "m", 10); err == nil {
		t.Fatal("empty token")
	}
	if _, err := c.GetMedia(context.Background(), "t", "", 10); err == nil {
		t.Fatal("empty media_id")
	}
}

func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
