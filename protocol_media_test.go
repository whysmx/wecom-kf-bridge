package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMediaGet_RequiresAuth(t *testing.T) {
	e := newEnv(t)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cgi-bin/media/get?media_id=MID", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["errcode"] != float64(ErrCodeAuth) {
		t.Fatalf("body=%v", body)
	}
	if strings.Contains(rr.Body.String(), "MID") {
		t.Fatal("media_id leaked in error body")
	}
}

func TestMediaGet_ProxySuccess(t *testing.T) {
	e := newEnv(t)
	e.ad.media = map[string]MediaDownload{
		"MID99": {Data: []byte("PNGDATA"), ContentType: "image/png", ContentDisposition: `attachment; filename="a.png"`},
	}
	tok := issueTestToken(t, e)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cgi-bin/media/get?access_token="+tok+"&media_id=MID99", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "PNGDATA" {
		t.Fatalf("body=%q", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("ct=%s", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "a.png") {
		t.Fatalf("cd=%s", cd)
	}
}

func TestMediaGet_MissingMediaID(t *testing.T) {
	e := newEnv(t)
	tok := issueTestToken(t, e)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cgi-bin/media/get?access_token="+tok, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rr.Code)
	}
}

func TestMediaGet_AdapterErrorNoLeak(t *testing.T) {
	e := newEnv(t)
	e.ad.mediaErr = errAPI{ErrCodeUnavailable, "upstream down", http.StatusBadGateway}
	tok := issueTestToken(t, e)
	rr := httptest.NewRecorder()
	secretMedia := "SUPER_SECRET_MEDIA_ID_VALUE_XYZ"
	e.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cgi-bin/media/get?access_token="+tok+"&media_id="+secretMedia, nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secretMedia) || strings.Contains(rr.Body.String(), tok) {
		t.Fatalf("leaked secrets in body: %s", rr.Body.String())
	}
}

func issueTestToken(t *testing.T, e *env) string {
	t.Helper()
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("gettoken %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
		t.Fatalf("token parse %v body=%s", err, rr.Body.String())
	}
	return out.AccessToken
}
