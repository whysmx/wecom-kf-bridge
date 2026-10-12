package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// tokenCache caches one enterprise's real access_token. Concurrent callers
// share a single refresh; the real token never leaves this process.
type tokenCache struct {
	client *wecom.Client
	now    func() time.Time
	mu     sync.Mutex
	token  string
	exp    time.Time
}

func (t *tokenCache) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.exp) {
		return t.token, nil
	}
	r, err := t.client.GetToken(ctx)
	if err != nil {
		return "", err
	}
	life := time.Duration(r.ExpiresIn) * time.Second
	if life > 2*time.Minute {
		life -= time.Minute
	}
	t.token, t.exp = r.AccessToken, t.now().Add(life)
	return t.token, nil
}
func (t *tokenCache) invalidate(tok string) {
	t.mu.Lock()
	if t.token == tok {
		t.token = ""
	}
	t.mu.Unlock()
}

// tokenRejected reports WeCom errcodes for an invalid/expired access_token:
// the request was refused before execution, so one refresh+retry is safe.
func tokenRejected(err error) bool {
	var a *wecom.APIError
	return errors.As(err, &a) && (a.Code == 40014 || a.Code == 42001 || a.Code == 40001)
}

// errTokenFetch: the real token could not be obtained, so no business
// request was sent.
var errTokenFetch = errors.New("runtime: access token unavailable")

type bindingRoute struct {
	enterpriseID string
	openKfID     string
}

// WeComAdapter implements bridge.Adapter against the real customer service
// API. It is the only production adapter.
type WeComAdapter struct {
	clients map[string]*wecom.Client // enterprise id
	tokens  map[string]*tokenCache
	routes  map[string]bindingRoute // binding id
	states  map[int]string
	store   *state.Store
	now     func() time.Time
	rmu     sync.RWMutex
	logger  Logger
	// treat48002AsAI: joint-debug only; see Config.WeCom.Treat48002AsAIEligible.
	treat48002AsAI bool
}

func (a *WeComAdapter) log(ev string, f map[string]any) {
	if a.logger != nil {
		a.logger.Log(ev, f)
	}
}

func (a *WeComAdapter) setRoute(bindingID string, r bindingRoute) {
	a.rmu.Lock()
	a.routes[bindingID] = r
	a.rmu.Unlock()
}

func (a *WeComAdapter) route(bindingID string) (bindingRoute, *wecom.Client, *tokenCache, error) {
	a.rmu.RLock()
	r, ok := a.routes[bindingID]
	a.rmu.RUnlock()
	if !ok {
		return r, nil, nil, errors.New("runtime: unknown binding")
	}
	return r, a.clients[r.enterpriseID], a.tokens[r.enterpriseID], nil
}

// withToken runs fn with the real token, refreshing once if WeCom says the
// token is invalid (request not executed).
func withToken[T any](ctx context.Context, tc *tokenCache, fn func(string) (T, error)) (T, error) {
	tok, err := tc.get(ctx)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %v", errTokenFetch, err)
	}
	v, err := fn(tok)
	if tokenRejected(err) {
		tc.invalidate(tok)
		if tok, err = tc.get(ctx); err != nil {
			return v, fmt.Errorf("%w: %v", errTokenFetch, err)
		}
		v, err = fn(tok)
	}
	return v, err
}

func (a *WeComAdapter) GetUser(ctx context.Context, b bridge.Binding, c bridge.Customer) (bridge.Customer, error) {
	_, cl, tc, err := a.route(b.ID)
	if err != nil {
		return c, err
	}
	resp, err := withToken(ctx, tc, func(tok string) (wecom.CustomerBatchGetResponse, error) {
		return cl.BatchGetCustomer(ctx, tok, []string{c.ExternalUserID})
	})
	if err != nil {
		return c, err
	}
	for _, p := range resp.Customers {
		if p.ExternalUserID == c.ExternalUserID && p.Nickname != "" {
			c.Name = p.Nickname
			_ = a.store.SetNickname(ctx, c.ID, p.Nickname, a.now())
		}
	}
	return c, nil
}

// ServiceState queries kf/service_state/get and maps the official value.
// Failures are logged with a distinct event per cause; logs carry the
// binding, the official errcode/errmsg and the raw state integer, never
// tokens, external user ids or message bodies.
func (a *WeComAdapter) ServiceState(ctx context.Context, b bridge.Binding, c bridge.Customer) (string, error) {
	r, cl, tc, err := a.route(b.ID)
	if err != nil {
		a.log("service_state_route_error", map[string]any{"binding_id": b.ID, "error": err.Error()})
		return "", err
	}
	resp, err := withToken(ctx, tc, func(tok string) (wecom.ServiceStateResponse, error) {
		return cl.GetServiceState(ctx, tok, wecom.ServiceStateRequest{OpenKfID: r.openKfID, ExternalUserID: c.ExternalUserID})
	})
	if err != nil {
		var ae *wecom.APIError
		if errors.As(err, &ae) {
			f := map[string]any{"binding_id": b.ID, "open_kfid": r.openKfID, "errcode": ae.Code, "errmsg": ae.Message, "http_status": ae.HTTPStatus}
			if ae.Code == 48002 {
				f["hint"] = "api forbidden: the calling app must be in 微信客服「可调用接口的应用」, the kf account must be set to 通过API管理, and its servicers must be in the app's visible range"
				if a.treat48002AsAI {
					// Distinguishable from a real official AI mapping: send
					// may proceed, auto-recover must not clear a fence.
					f["effect"] = "treat_48002_as_ai_eligible: send may proceed as AI_ELIGIBLE_48002_BYPASS; auto-recover will not clear fences (joint-debug only; never production)"
					a.log("WARNING_service_state_48002_bypassed", f)
					return state.CustomerAIEligibleBypass48002, nil
				}
			}
			a.log("service_state_upstream_errcode", f)
		} else {
			a.log("service_state_call_error", map[string]any{"binding_id": b.ID, "error": err.Error()})
		}
		return "", err
	}
	if st, ok := a.states[resp.ServiceState]; ok {
		if st != state.CustomerAIEligible {
			a.log("service_state_not_ai", map[string]any{"binding_id": b.ID, "service_state": resp.ServiceState, "mapped": st})
		}
		return st, nil
	}
	a.log("service_state_unmapped", map[string]any{"binding_id": b.ID, "service_state": resp.ServiceState, "mapped": state.CustomerUnknown})
	return state.CustomerUnknown, nil
}

func (a *WeComAdapter) GetMedia(ctx context.Context, b bridge.Binding, mediaID string) (bridge.MediaDownload, error) {
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return bridge.MediaDownload{}, fmt.Errorf("runtime: media_id required")
	}
	_, cl, tc, err := a.route(b.ID)
	if err != nil {
		return bridge.MediaDownload{}, err
	}
	return withToken(ctx, tc, func(tok string) (bridge.MediaDownload, error) {
		md, err := cl.GetMedia(ctx, tok, mediaID, bridge.MaxMediaBytes)
		if err != nil {
			return bridge.MediaDownload{}, err
		}
		return bridge.MediaDownload{Data: md.Data, ContentType: md.ContentType, ContentDisposition: md.ContentDisposition}, nil
	})
}

func (a *WeComAdapter) SendText(ctx context.Context, req bridge.SendRequest) (string, error) {
	r, cl, tc, err := a.route(req.BindingID)
	if err != nil {
		return "", &bridge.SendError{Kind: bridge.SendUnavailable, Err: err}
	}
	resp, err := withToken(ctx, tc, func(tok string) (wecom.SendResponse, error) {
		return cl.SendMsg(ctx, tok, wecom.SendRequest{ToUser: req.Customer.ExternalUserID, OpenKfID: r.openKfID, MsgType: "text", Text: &wecom.SendText{Content: req.Content}})
	})
	if err == nil {
		return resp.MsgID, nil
	}
	return "", classifySendError(err)
}

func classifySendError(err error) error {
	var a *wecom.APIError
	var op *net.OpError
	var dns *net.DNSError
	switch {
	case errors.As(err, &a) && a.Code > 0 && a.HTTPStatus < 500:
		// explicit WeCom errcode on a delivered response: refused
		return &bridge.SendError{Kind: bridge.SendRejected, Err: errors.New("wecom errcode " + strconv.Itoa(a.Code))}
	case errors.As(err, &op) && op.Op == "dial", errors.As(err, &dns), errors.Is(err, wecom.ErrInvalidArgument), errors.Is(err, errTokenFetch):
		return &bridge.SendError{Kind: bridge.SendUnavailable, Err: err}
	}
	return &bridge.SendError{Kind: bridge.SendUnknown, Err: err}
}
