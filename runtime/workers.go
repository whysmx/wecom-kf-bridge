package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// Worker is a background loop started with the HTTP server and stopped
// (context cancelled, then awaited up to the shutdown grace) on shutdown.
type Worker interface {
	Run(ctx context.Context)
}

type scopeInfo struct {
	enterpriseID, bindingID, openKfID string
	client                            *wecom.Client
	tokens                            *tokenCache
}

// SyncWorker pulls sync_msg per scope. Notifications wake it; a ticker
// resumes pending scopes (crash, page limit). Different scopes run
// concurrently up to MaxConc; one scope never runs twice at once.
type SyncWorker struct {
	Store    *state.Store
	Scopes   map[string]scopeInfo
	Origins  wecom.OriginPolicy
	Interval time.Duration
	MaxPages int
	MaxConc  int
	Logger   Logger
	OnSynced func()

	wake    chan string
	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
	smu     sync.RWMutex
}

// SetScope registers a scope at runtime (admin-created binding).
func (w *SyncWorker) SetScope(scope string, info scopeInfo) {
	w.smu.Lock()
	defer w.smu.Unlock()
	w.Scopes[scope] = info
}

func (w *SyncWorker) init() {
	w.mu.Lock()
	if w.wake == nil {
		w.wake = make(chan string, 256)
		w.running, w.again = map[string]bool{}, map[string]bool{}
	}
	w.mu.Unlock()
}

// Wake schedules a scope; it never blocks the callback handler.
func (w *SyncWorker) Wake(scope string) {
	w.init()
	select {
	case w.wake <- scope:
	default: // full: the scope is already marked pending in SQLite
	}
}

func (w *SyncWorker) Run(ctx context.Context) {
	w.init()
	sem := make(chan struct{}, max(1, w.MaxConc))
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	start := func(id string) {
		w.mu.Lock()
		if w.running[id] {
			w.again[id] = true
			w.mu.Unlock()
			return
		}
		w.running[id] = true
		w.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					w.finish(id)
					return
				}
				w.SyncOnce(ctx, id)
				<-sem
				w.mu.Lock()
				if !w.again[id] || ctx.Err() != nil {
					delete(w.running, id)
					w.mu.Unlock()
					return
				}
				delete(w.again, id)
				w.mu.Unlock()
			}
		}()
	}
	w.resumePending(ctx, start)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.wake:
			start(id)
		case <-t.C:
			w.resumePending(ctx, start)
		}
	}
}
func (w *SyncWorker) finish(id string) {
	w.mu.Lock()
	delete(w.running, id)
	w.mu.Unlock()
}
func (w *SyncWorker) resumePending(ctx context.Context, start func(string)) {
	sc, err := w.Store.PendingScopes(ctx, 100)
	if err != nil {
		w.Logger.Log("sync_pending_query_failed", map[string]any{"error": err})
		return
	}
	for _, s := range sc {
		start(s.ID)
	}
}

// SyncOnce runs one SyncAll for a scope.
func (w *SyncWorker) SyncOnce(ctx context.Context, scope string) {
	w.smu.RLock()
	info, ok := w.Scopes[scope]
	w.smu.RUnlock()
	if !ok {
		return
	}
	tok, err := info.tokens.get(ctx)
	if err != nil {
		w.Logger.Log("sync_token_unavailable", map[string]any{"scope": scope, "error": err})
		return
	}
	pull, err := w.Store.SyncToken(ctx, scope)
	if err != nil {
		w.Logger.Log("sync_pull_token_unreadable", map[string]any{"scope": scope, "error": err})
		return
	}
	res, err := info.client.SyncAll(ctx, wecom.SyncOptions{Scope: scope, EnterpriseID: info.enterpriseID, BindingID: info.bindingID, OpenKfID: info.openKfID, AccessToken: tok, Request: wecom.SyncRequest{Token: pull, OpenKfID: info.openKfID}, Store: w.Store, Origins: w.Origins, MaxPages: w.MaxPages, Logger: w.Logger})
	if err != nil {
		w.Logger.Log("sync_failed", map[string]any{"scope": scope, "error": err})
		return
	}
	if res.Inserted > 0 && w.OnSynced != nil {
		w.OnSynced()
	}
}

// DeliveryWorker posts stored customer messages to cc-connect as encrypted
// XML callbacks. Customers are processed independently and concurrently (up
// to MaxConc); messages of one customer stay in order.
type DeliveryWorker struct {
	Store       *state.Store
	Server      *bridge.Server
	CallbackURL map[string]string // binding id
	AgentID     map[string]string
	HTTP        *http.Client
	Interval    time.Duration
	MaxAttempts int
	MaxConc     int
	Logger      Logger
	// OfficialState queries the official service_state (mapped) for an
	// automatically fenced customer; nil disables automatic recovery.
	OfficialState func(ctx context.Context, bindingID string, c state.Customer) (string, error)
	// HandoverPhrase exact-match (trim) triggers intentional human handoff.
	HandoverPhrase string
	// OnHandoverPhrase runs the official+local handoff; nil disables 口令.
	OnHandoverPhrase func(ctx context.Context, c state.Customer, m state.InboxMessage) error

	kick chan struct{}
	once sync.Once
	tmu  sync.RWMutex
}

// SetTarget updates a binding's callback URL/agent at runtime (admin).
func (d *DeliveryWorker) SetTarget(bindingID, url, agentID string) {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	d.CallbackURL[bindingID], d.AgentID[bindingID] = url, agentID
}

func (d *DeliveryWorker) target(bindingID string) (string, string) {
	d.tmu.RLock()
	defer d.tmu.RUnlock()
	return d.CallbackURL[bindingID], d.AgentID[bindingID]
}

func (d *DeliveryWorker) init() { d.once.Do(func() { d.kick = make(chan struct{}, 1) }) }

// Kick requests an immediate delivery pass.
func (d *DeliveryWorker) Kick() {
	d.init()
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *DeliveryWorker) Run(ctx context.Context) {
	d.init()
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	for {
		d.DeliverOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
	}
}

// DeliverOnce performs one bounded pass.
func (d *DeliveryWorker) DeliverOnce(ctx context.Context) {
	ms, err := d.Store.InboxForDelivery(ctx, d.MaxAttempts, 200)
	if err != nil {
		d.Logger.Log("delivery_query_failed", map[string]any{"error": err})
		return
	}
	byCustomer := map[string][]state.InboxMessage{}
	var order []string
	for _, m := range ms {
		if _, ok := byCustomer[m.CustomerID]; !ok {
			order = append(order, m.CustomerID)
		}
		byCustomer[m.CustomerID] = append(byCustomer[m.CustomerID], m)
	}
	sem := make(chan struct{}, max(1, d.MaxConc))
	var wg sync.WaitGroup
	for _, cid := range order {
		list := byCustomer[cid]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			for _, m := range list {
				if ctx.Err() != nil || !d.deliver(ctx, m) {
					return // keep per-customer order: stop at first non-final
				}
			}
		}()
	}
	wg.Wait()
}

// autoRecover: a customer fenced automatically (from an official
// service_state observation) writes again. If the official state is now AI
// (0/1), clear the fence with a new generation and deliver this message
// under the new UID. Manual takeovers never get here (FenceSource manual).
func (d *DeliveryWorker) autoRecover(ctx context.Context, c state.Customer, m state.InboxMessage) (state.Customer, state.InboxMessage) {
	if d.OfficialState == nil {
		return c, m
	}
	st, err := d.OfficialState(ctx, m.BindingID, c)
	if err != nil || st != state.CustomerAIEligible {
		return c, m
	}
	nc, recovered, err := d.Store.ObserveOfficialAI(ctx, c.ID)
	if err != nil {
		d.Logger.Log("auto_recover_failed", map[string]any{"customer_id": c.ID, "error": err.Error()})
		return c, m
	}
	if nc.Generation == m.Generation+1 || recovered {
		if err := d.Store.RebindInboxGeneration(ctx, m.ID, m.Generation, nc.Generation); err != nil {
			return nc, m
		}
		m.Generation = nc.Generation
	}
	if recovered {
		d.Logger.Log("customer_auto_recovered", map[string]any{"customer_id": c.ID, "generation": nc.Generation})
	}
	return nc, m
}

const stepTimeout = 5 * time.Second

// buildInboundCallback maps an inbox row to the cc-connect WeCom XML contract:
// text keeps Content; image/file/voice carry MsgType + MediaId (+ FileName for file).
func buildInboundCallback(uid, agentID string, m state.InboxMessage) (bridge.InboundMessage, error) {
	msg := bridge.InboundMessage{
		FromUserName: uid,
		CreateTime:   m.CreateTime.Unix(),
		MsgType:      m.Type,
		MsgID:        fmt.Sprint(m.CompatMsgID),
		AgentID:      agentID,
	}
	switch strings.ToLower(strings.TrimSpace(m.Type)) {
	case "text":
		msg.Content = m.PayloadRef
		return msg, nil
	case "image", "voice", "file":
		mp, ok := bridge.DecodeMediaPayload(m.PayloadRef)
		if !ok || mp.MediaID == "" {
			return msg, fmt.Errorf("missing_media_id")
		}
		msg.MediaID = mp.MediaID
		if strings.EqualFold(m.Type, "file") {
			msg.FileName = bridge.SafeFileName(mp.FileName)
		}
		return msg, nil
	default:
		return msg, fmt.Errorf("type:%s", m.Type)
	}
}

// deliver advances one message; false means later messages of the same
// customer must wait.
func (d *DeliveryWorker) deliver(ctx context.Context, m state.InboxMessage) bool {
	step := func(to, cat string) bool {
		// Record the outcome even if shutdown cancelled ctx meanwhile: after a
		// callback was accepted, losing the transition would leave the message
		// POSTING and redeliver it after restart (issue #5).
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
		defer cancel()
		_, err := d.Store.TransitionInbox(sctx, m.ID, to, cat)
		if err != nil {
			d.Logger.Log("delivery_transition_failed", map[string]any{"inbox_id": m.ID, "to": to, "error": err})
			return false
		}
		m.State = to
		return true
	}
	if m.State == state.InboxReceived {
		if !bridge.DeliverableMsgType(m.Type) {
			return step(state.InboxUnsupported, "type:"+m.Type)
		}
		if m.Type != "text" {
			if mp, ok := bridge.DecodeMediaPayload(m.PayloadRef); !ok || mp.MediaID == "" {
				return step(state.InboxUnsupported, "missing_media_id")
			}
		}
		if !step(state.InboxClassified, "") {
			return false
		}
	}
	if m.State == state.InboxClassified && !step(state.InboxReady, "") {
		return false
	}
	c, err := d.Store.Customer(ctx, m.CustomerID)
	if err != nil {
		return false
	}
	if d.OnHandoverPhrase != nil && m.Type == "text" && MatchHandoverPhrase(HandoverConfig{Phrase: d.HandoverPhrase}, m.PayloadRef) {
		if err := d.OnHandoverPhrase(ctx, c, m); err != nil {
			d.Logger.Log("handover_phrase_failed", map[string]any{"customer_id": c.ID, "inbox_id": m.ID, "error": err.Error()})
			return step(state.InboxHeld, "handover_phrase_failed")
		}
		d.Logger.Log("handover_phrase_ok", map[string]any{"customer_id": c.ID, "inbox_id": m.ID})
		return step(state.InboxHeld, "handover_phrase")
	}
	if c.State != state.CustomerAIEligible && c.FenceSource == state.FenceAuto && c.Authorized && c.Generation == m.Generation {
		c, m = d.autoRecover(ctx, c, m)
	}
	if c.State != state.CustomerAIEligible || !c.Authorized || c.Generation != m.Generation {
		return step(state.InboxHeld, "customer_not_eligible")
	}
	if m.CreateTime.IsZero() {
		// Never substitute the current time or 0 (docs/03 §6).
		return step(state.InboxHeld, "missing_create_time")
	}
	target, agentID := d.target(m.BindingID)
	if target == "" {
		return step(state.InboxHeld, "no_callback_url")
	}
	inbound, err := buildInboundCallback(c.UID, agentID, m)
	if err != nil {
		return step(state.InboxUnsupported, err.Error())
	}
	cb, err := d.Server.BuildCallback(m.BindingID, inbound)
	if err != nil {
		return step(state.InboxHeld, "build_failed")
	}
	if !step(state.InboxPosting, "") {
		return false
	}
	q := url.Values{"msg_signature": {cb.Signature}, "timestamp": {cb.Timestamp}, "nonce": {cb.Nonce}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"?"+q.Encode(), bytes.NewReader(cb.Body))
	if err != nil {
		step(state.InboxRetryWait, "request")
		return false
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := d.HTTP.Do(req)
	switch {
	case err != nil && provablyNotDelivered(err):
		step(state.InboxRetryWait, "connect")
		return false
	case err != nil:
		step(state.InboxDeliveryUnknown, "transport")
		return false
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return step(state.InboxHTTPAccepted, "")
	}
	step(state.InboxDeliveryUnknown, fmt.Sprintf("http_%d", resp.StatusCode))
	return false
}

func provablyNotDelivered(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, errTargetNotAllowed) || errors.Is(err, syscall.ECONNREFUSED)
}

var errTargetNotAllowed = errors.New("runtime: callback target address not allowed")

// callbackHTTPClient enforces a per-target dial policy (docs/09 §5): the
// request's hostname:port must be a configured target, the name is
// resolved at dial time, and only addresses inside THAT target's CIDRs are
// dialled. A target therefore cannot reach another target's network even
// on the same port. Proxies and redirects are disabled.
func callbackHTTPClient(cfg Config) *http.Client {
	return newCallbackClient(cfg, net.DefaultResolver.LookupIPAddr)
}

type lookupFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func newCallbackClient(cfg Config, lookup lookupFunc) *http.Client {
	type policy struct{ nets []*net.IPNet }
	policies := map[string]policy{}
	for _, t := range cfg.Security.CallbackTargets {
		key := net.JoinHostPort(strings.ToLower(t.Host), fmt.Sprint(t.Port))
		p := policies[key]
		for _, c := range t.AllowedCIDRs {
			if _, n, err := net.ParseCIDR(c); err == nil {
				p.nets = append(p.nets, n)
			}
		}
		policies[key] = p
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		p, ok := policies[net.JoinHostPort(strings.ToLower(host), port)]
		if !ok {
			return nil, errTargetNotAllowed
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			addrs, err := lookup(ctx, host)
			if err != nil {
				return nil, &net.OpError{Op: "dial", Net: network, Err: err}
			}
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		for _, ip := range ips {
			if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
				continue
			}
			for _, n := range p.nets {
				if n.Contains(ip) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				}
			}
		}
		return nil, errTargetNotAllowed
	}
	tr := &http.Transport{DialContext: dial, Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
