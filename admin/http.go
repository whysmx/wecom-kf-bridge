package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func (c *Console) Handler() http.Handler { return c.mux }

// result of a write: where to redirect (PRG) and the message to flash, or
// an HTTP status for refusals (409 revision conflict etc.).
type result struct {
	location, flash string
	status          int
}

type writeFn func(r *http.Request, s *session) result

type writeOpt struct{ stepUp bool }

func (c *Console) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /admin/login", c.loginPage)
	m.HandleFunc("POST /admin/login", c.login)
	m.Handle("POST /admin/logout", c.authed(c.post(c.logout, writeOpt{}, false)))
	m.Handle("POST /admin/stepup", c.authed(c.post(c.stepUp, writeOpt{}, false)))
	for path, h := range map[string]func(http.ResponseWriter, *http.Request, *session){
		"GET /admin/{$}":         c.overview,
		"GET /admin/accounts":    c.accountsPage,
		"GET /admin/bindings":    c.bindingsPage,
		"GET /admin/customers":   c.customersPage,
		"GET /admin/diagnostics": c.diagnosticsPage,
		"GET /admin/settings":    c.settingsPage,
		"GET /admin/audit":       c.auditPage,
	} {
		m.Handle(path, c.authed(h))
	}
	writes := map[string]struct {
		fn  writeFn
		opt writeOpt
	}{
		"POST /admin/accounts/sync":                {c.accountsSync, writeOpt{}},
		"POST /admin/accounts/create":              {c.accountCreate, writeOpt{}},
		"POST /admin/accounts/{id}/edit":           {c.accountEdit, writeOpt{}},
		"POST /admin/accounts/{id}/link":           {c.accountLink, writeOpt{}},
		"POST /admin/accounts/{id}/delete":         {c.accountDelete, writeOpt{stepUp: true}},
		"POST /admin/bindings/create":              {c.bindingCreate, writeOpt{}},
		"POST /admin/bindings/{id}/enable":         {c.bindingEnable, writeOpt{}},
		"POST /admin/bindings/{id}/disable":        {c.bindingDisable, writeOpt{}},
		"POST /admin/bindings/{id}/rebind":         {c.bindingRebind, writeOpt{stepUp: true}},
		"POST /admin/bindings/{id}/rotate":         {c.bindingRotate, writeOpt{stepUp: true}},
		"POST /admin/bindings/{id}/verify":         {c.bindingVerify, writeOpt{}},
		"POST /admin/bindings/{id}/export":         {c.bindingExport, writeOpt{stepUp: true}},
		"POST /admin/customers/{id}/handover":      {c.customerHandover, writeOpt{}},
		"POST /admin/customers/{id}/recover":       {c.customerRecover, writeOpt{}},
		"POST /admin/diagnostics/{kind}/{id}/mark": {c.diagMark, writeOpt{}},
		"POST /admin/settings/test":                {c.settingsTest, writeOpt{}},
	}
	for path, w := range writes {
		m.Handle(path, c.authed(c.post(w.fn, w.opt, true)))
	}
	// Viewing a full message body: step-up, audited, rendered directly
	// (never via URL/redirect), no-store.
	m.Handle("POST /admin/diagnostics/{kind}/{id}/view", c.authed(c.viewBody))
	c.mux = m
}

// secure sets headers on every console response: no caching, no framing,
// no third-party content.
func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

type authedFn = func(http.ResponseWriter, *http.Request, *session)

// authed rejects every request without a valid session: GET redirects to
// the login page, anything else is 401.
func (c *Console) authed(h authedFn) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		s := c.lookup(r)
		if s == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "未登录", http.StatusUnauthorized)
			return
		}
		h(w, r, s)
	})
}

// sameOrigin requires the Origin (or, without it, the Referer) to be the
// configured console origin.
func (c *Console) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == c.cfg.Origin
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && ref.Scheme+"://"+ref.Host == c.cfg.Origin
}

func (c *Console) csrfOK(r *http.Request, s *session) bool {
	return subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf_token")), []byte(s.csrf)) == 1
}

// paramsHash fingerprints a write (path + form without the CSRF token,
// idempotency key and password) for Idempotency-Key replay detection.
func paramsHash(r *http.Request) string {
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		if k != "csrf_token" && k != "idempotency_key" && k != "password" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", r.URL.Path)
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q\n", k, r.PostForm[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func idemKey(r *http.Request) string {
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		return k
	}
	return r.PostFormValue("idempotency_key")
}

// post wraps a write: body limit, origin + CSRF, optional step-up, and
// (when idempotent) Idempotency-Key reservation/replay. The result is a
// 303 redirect (PRG) or a refusal status.
func (c *Console) post(fn writeFn, opt writeOpt, idempotent bool) authedFn {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "请求无效", http.StatusBadRequest)
			return
		}
		if !c.sameOrigin(r) || !c.csrfOK(r, s) {
			http.Error(w, "CSRF 校验失败", http.StatusForbidden)
			return
		}
		if opt.stepUp && !c.steppedUp(s) {
			http.Error(w, "此操作需要重新输入密码（二次认证）", http.StatusForbidden)
			return
		}
		var key string
		if idempotent {
			key = idemKey(r)
			if len(key) < 16 || len(key) > 128 {
				http.Error(w, "缺少 Idempotency-Key", http.StatusBadRequest)
				return
			}
			op, err := c.cfg.Store.BeginOperation(r.Context(), key, paramsHash(r))
			switch {
			case errors.Is(err, state.ErrIdempotencyMismatch), errors.Is(err, state.ErrOperationInProgress):
				http.Error(w, "Idempotency-Key 冲突", http.StatusConflict)
				return
			case err != nil:
				http.Error(w, "存储不可用", http.StatusServiceUnavailable)
				return
			case op != nil:
				replay(w, r, op.Location)
				return
			}
		}
		res := fn(r, s)
		loc := res.location
		if res.status != 0 {
			loc = "status:" + strconv.Itoa(res.status) + ":" + res.flash
		}
		if idempotent {
			_ = c.cfg.Store.FinishOperation(r.Context(), key, loc)
		}
		if res.flash != "" && res.status == 0 {
			c.setFlash(s, res.flash)
		}
		replay(w, r, loc)
	}
}

func replay(w http.ResponseWriter, r *http.Request, loc string) {
	if rest, ok := strings.CutPrefix(loc, "status:"); ok {
		code, msg, _ := strings.Cut(rest, ":")
		n, _ := strconv.Atoi(code)
		http.Error(w, msg, n)
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

func (c *Console) audit(r *http.Request, objType, objID, op, outcome, summary string, rev int64) {
	key := idemKey(r)
	if key == "" {
		key = c.randomHex(16)
	}
	_ = c.cfg.Store.AddActorAudit(r.Context(), state.AuditEntry{Actor: "admin@" + clientIP(r), Key: op + ":" + key, ObjectType: objType, ObjectID: objID, Operation: op, Result: outcome, Summary: summary, Revision: rev})
}

// clientIP is the TCP peer; X-Forwarded-For is not trusted (docs/17 §2).
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func formRev(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	return n
}

func conflict(msg string) result {
	return result{status: http.StatusConflict, flash: msg + "（revision 已变化，请刷新后重新确认）"}
}

func (c *Console) loginPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	c.render(w, "login", nil, nil)
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	secure(w)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil || !c.sameOrigin(r) {
		http.Error(w, "请求无效", http.StatusForbidden)
		return
	}
	ok, locked := c.checkPassword(r.PostFormValue("password"))
	if !ok {
		outcome := "failed"
		if locked {
			outcome = "locked"
		}
		c.audit(r, "session", "admin", "login", outcome, "", 0)
		w.WriteHeader(http.StatusUnauthorized)
		c.render(w, "login", nil, map[string]any{"Error": "密码错误或尝试过多，请稍后再试"})
		return
	}
	s := c.newSession()
	c.audit(r, "session", "admin", "login", "ok", "", 0)
	c.setCookie(w, s.id, int(c.cfg.SessionTTL.Seconds()))
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (c *Console) logout(r *http.Request, s *session) result {
	c.drop(s)
	c.audit(r, "session", "admin", "logout", "ok", "", 0)
	return result{location: "/admin/login"}
}

func (c *Console) stepUp(r *http.Request, s *session) result {
	next := r.PostFormValue("next")
	if !strings.HasPrefix(next, "/admin/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		next = "/admin/"
	}
	ok, _ := c.checkPassword(r.PostFormValue("password"))
	if !ok {
		c.audit(r, "session", "admin", "step_up", "failed", "", 0)
		return result{status: http.StatusUnauthorized, flash: "二次认证失败"}
	}
	c.mu.Lock()
	s.stepUpUntil = c.cfg.Clock().Add(c.cfg.StepUpTTL)
	c.mu.Unlock()
	c.audit(r, "session", "admin", "step_up", "ok", "", 0)
	return result{location: next, flash: "已通过二次认证"}
}
