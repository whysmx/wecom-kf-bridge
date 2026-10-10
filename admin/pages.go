package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func pageNum(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func storeErr(w http.ResponseWriter) { http.Error(w, "存储不可用", http.StatusServiceUnavailable) }

func (c *Console) overview(w http.ResponseWriter, r *http.Request, s *session) {
	counts, err := c.cfg.Store.Overview(r.Context())
	if err != nil {
		storeErr(w)
		return
	}
	c.renderPage(w, r, s, "overview", map[string]any{"Counts": counts})
}

const pageSize = 20

func (c *Console) accountsPage(w http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query().Get("q")
	if len(q) > 64 {
		q = q[:64]
	}
	p := pageNum(r)
	list, total, err := c.cfg.Store.Accounts(r.Context(), q, (p-1)*pageSize, pageSize)
	if err != nil {
		storeErr(w)
		return
	}
	bindings, err := c.cfg.Store.Bindings(r.Context())
	if err != nil {
		storeErr(w)
		return
	}
	projects := map[string]string{}
	for _, b := range bindings {
		if b.EnterpriseID == c.cfg.EnterpriseID {
			projects[b.OpenKfID] = b.ProjectID
		}
	}
	data := map[string]any{"Accounts": list, "Total": total, "Q": q, "Page": p, "Projects": projects}
	if p > 1 {
		data["Prev"] = p - 1
	}
	if p*pageSize < total {
		data["Next"] = p + 1
	}
	if id := r.URL.Query().Get("id"); id != "" {
		a, err := c.cfg.Store.Account(r.Context(), id)
		if errors.Is(err, state.ErrNotFound) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			storeErr(w)
			return
		}
		data["Detail"] = a
		data["Ticket"] = c.issueTicket(s, a.OpenKfID, "delete", a.Revision)
	}
	c.renderPage(w, r, s, "accounts", data)
}

func (c *Console) bindingsPage(w http.ResponseWriter, r *http.Request, s *session) {
	all, err := c.cfg.Store.Bindings(r.Context())
	if err != nil {
		storeErr(w)
		return
	}
	var mine []state.Binding
	for _, b := range all {
		if b.EnterpriseID == c.cfg.EnterpriseID {
			mine = append(mine, b)
		}
	}
	c.renderPage(w, r, s, "bindings", map[string]any{"Bindings": mine})
}

func (c *Console) customersPage(w http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query().Get("q")
	list, err := c.cfg.Store.Customers(r.Context(), q, 50)
	if err != nil {
		storeErr(w)
		return
	}
	var mine []state.Customer
	for _, cu := range list {
		if cu.EnterpriseID == c.cfg.EnterpriseID {
			mine = append(mine, cu)
		}
	}
	data := map[string]any{"Customers": mine, "Q": q}
	if r.URL.Query().Get("detail") == "1" && len(mine) == 1 {
		in, err1 := c.cfg.Store.InboxByStates(r.Context(), mine[0].ID, nil, 20)
		out, err2 := c.cfg.Store.OutboxByStates(r.Context(), mine[0].ID, nil, 20)
		if err1 != nil || err2 != nil {
			storeErr(w)
			return
		}
		data["Detail"], data["Inbox"], data["Outbox"] = true, in, out
	}
	c.renderPage(w, r, s, "customers", data)
}

type diagItem struct {
	Kind, ID, Customer, State, Chunks, Error, Mark string
	Attempt                                        int
	Revision                                       int64
	At                                             time.Time
}

var (
	inboxFocus  = []string{state.InboxRetryWait, state.InboxHeld, state.InboxDeliveryUnknown}
	outboxFocus = []string{state.OutboxUnknown, state.OutboxUpstreamAccepted, state.OutboxRejected}
)

func (c *Console) diagnosticsPage(w http.ResponseWriter, r *http.Request, s *session) {
	view := r.URL.Query().Get("view")
	ctx := r.Context()
	var items []diagItem
	inMarks, err1 := c.cfg.Store.DiagnosticMarks(ctx, "inbox")
	outMarks, err2 := c.cfg.Store.DiagnosticMarks(ctx, "outbox")
	if err1 != nil || err2 != nil {
		storeErr(w)
		return
	}
	if view != "outbox" {
		in, err := c.cfg.Store.InboxByStates(ctx, "", inboxFocus, 100)
		if err != nil {
			storeErr(w)
			return
		}
		for _, m := range in {
			id := strconv.FormatInt(m.ID, 10)
			if view == "pending" && m.State != state.InboxDeliveryUnknown {
				continue
			}
			items = append(items, diagItem{Kind: "inbox", ID: id, Customer: m.CustomerID, State: m.State, Attempt: m.Attempt, Error: m.ErrorCategory, At: m.UpdatedAt, Mark: inMarks[id]})
		}
	}
	if view != "inbox" {
		out, err := c.cfg.Store.OutboxByStates(ctx, "", outboxFocus, 100)
		if err != nil {
			storeErr(w)
			return
		}
		for _, o := range out {
			if view == "pending" && o.State != state.OutboxUnknown {
				continue
			}
			items = append(items, diagItem{Kind: "outbox", ID: o.ID, Customer: o.CustomerID, State: o.State, Attempt: o.Attempt, Chunks: fmt.Sprintf("%d/%d", o.ChunksSent, o.ChunksTotal), Error: o.ErrorCategory, Revision: o.BindingRevision, At: o.UpdatedAt, Mark: outMarks[o.ID]})
		}
	}
	c.renderPage(w, r, s, "diagnostics", map[string]any{"Items": items})
}

func (c *Console) settingsPage(w http.ResponseWriter, r *http.Request, s *session) {
	c.renderPage(w, r, s, "settings", map[string]any{"Settings": c.cfg.Settings})
}

func (c *Console) auditPage(w http.ResponseWriter, r *http.Request, s *session) {
	p := pageNum(r)
	list, total, err := c.cfg.Store.Audits(r.Context(), (p-1)*pageSize, pageSize)
	if err != nil {
		storeErr(w)
		return
	}
	data := map[string]any{"Entries": list, "Page": p}
	if p*pageSize < total {
		data["Next"] = p + 1
	}
	c.renderPage(w, r, s, "audit", data)
}

// viewBody shows one full message body after step-up; audited.
func (c *Console) viewBody(w http.ResponseWriter, r *http.Request, s *session) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil || !c.sameOrigin(r) || !c.csrfOK(r, s) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	if !c.steppedUp(s) {
		http.Error(w, "查看正文需要二次认证", http.StatusForbidden)
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	var body, customer string
	var err error
	switch kind {
	case "inbox":
		n, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil {
			http.NotFound(w, r)
			return
		}
		var m state.InboxMessage
		m, err = c.cfg.Store.Inbox(r.Context(), n)
		body, customer = m.PayloadRef, m.CustomerID
	case "outbox":
		var o state.OutboxMessage
		o, err = c.cfg.Store.Outbox(r.Context(), id)
		body, customer = o.Body, o.CustomerID
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil {
		err = c.inScope(r, customer)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.audit(r, kind, id, "view_body", "ok", "", 0)
	c.renderPage(w, r, s, "body", map[string]any{"Kind": kind, "ID": id, "Body": body})
}

// inScope checks the object belongs to this console's enterprise; a path
// ID can never widen the query boundary.
func (c *Console) inScope(r *http.Request, customerID string) error {
	cu, err := c.cfg.Store.Customer(r.Context(), customerID)
	if err != nil {
		return err
	}
	if cu.EnterpriseID != c.cfg.EnterpriseID {
		return state.ErrNotFound
	}
	return nil
}
