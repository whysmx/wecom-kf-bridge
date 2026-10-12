package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

var (
	ErrCursorNoProgress = errors.New("wecom: sync cursor made no progress")
	// ErrMissingMsgID pauses a scope: without the official msgid the gateway
	// cannot dedupe, so it never invents one (docs/04 §4: 无法确定消息边界则暂停).
	ErrMissingMsgID = errors.New("wecom: sync message without msgid; scope paused")
	// ErrOriginPolicy is returned when no customer origin is configured. The
	// numeric origin values are not verified by first-hand docs, so the
	// gateway refuses to guess a default (docs/04 §4, AT-019).
	ErrOriginPolicy = errors.New("wecom: customer origin policy not configured")
)

// Sync scope states visible to operators.
const (
	ScopeReady         = "READY"
	ScopeStalled       = "STALLED"
	ScopePageLimit     = "PAGE_LIMIT"
	ScopePausedNoMsgID = "PAUSED_MISSING_MSGID"
)

// SyncStore is the durable state SyncAll needs; *state.Store implements it.
type SyncStore interface {
	Scope(context.Context, string) (state.SyncScope, error)
	CommitSyncPage(context.Context, string, string, bool, []state.InboxMessage) (int, error)
	EnsureCustomer(context.Context, string, string, string) (state.Customer, error)
	SetSyncState(context.Context, string, string) error
}

// OriginPolicy lists the sync_msg origin values that are customer content.
// Everything else (servicer replies, system events, unknown origins) is
// recorded as IGNORED and never forwarded to cc-connect (AT-019).
type OriginPolicy struct{ CustomerOrigins []int }

func (p OriginPolicy) isCustomer(o int) bool {
	for _, v := range p.CustomerOrigins {
		if v == o {
			return true
		}
	}
	return false
}

type SyncOptions struct {
	Scope        string
	EnterpriseID string
	BindingID    string
	OpenKfID     string
	AccessToken  string
	Request      SyncRequest
	Store        SyncStore
	Origins      OriginPolicy
	MaxPages     int
	Logger       Logger
	Now          func() time.Time
}
type SyncResult struct {
	Scope    string
	Pages    int
	Messages int
	Inserted int
	Cursor   string
	HasMore  bool
	// Truncated is set when MaxPages was reached; the cursor and pending
	// flag are durable so the next run resumes from Cursor.
	Truncated bool
}

// Inbox error categories for non-customer records.
const (
	CategoryEvent         = "event"
	CategoryForeignKf     = "foreign_open_kfid"
	CategoryNoExternalUID = "missing_external_userid"
	// CategoryUnparseable quarantines a msg_list item that failed to decode.
	CategoryUnparseable = "unparseable"
)

func (o SyncOptions) classify(ctx context.Context, page SyncResponse, now time.Time) ([]state.InboxMessage, error) {
	out := make([]state.InboxMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		if m.MsgID == "" {
			return nil, ErrMissingMsgID
		}
		ct := time.Time{}
		if m.SendTime > 0 {
			ct = time.Unix(m.SendTime, 0)
		}
		payload := syncPayload(m)
		in := state.InboxMessage{ScopeID: o.Scope, BindingID: o.BindingID, ExternalMsgID: m.MsgID, CreateTime: ct, Source: fmt.Sprintf("origin:%d", m.Origin), Type: m.MsgType, ReceivedAt: now, UpdatedAt: now}
		switch {
		case o.OpenKfID != "" && m.OpenKfID != "" && m.OpenKfID != o.OpenKfID:
			in.State, in.ErrorCategory = state.InboxIgnored, CategoryForeignKf
		case m.MsgType == "event":
			in.State, in.ErrorCategory = state.InboxIgnored, CategoryEvent
		case !o.Origins.isCustomer(m.Origin):
			in.State, in.ErrorCategory = state.InboxIgnored, in.Source
		case m.ExternalUserID == "":
			in.State, in.ErrorCategory = state.InboxIgnored, CategoryNoExternalUID
		default:
			c, err := o.Store.EnsureCustomer(ctx, o.EnterpriseID, o.BindingID, m.ExternalUserID)
			if err != nil {
				return nil, err
			}
			in.CustomerID, in.Generation, in.CustomerInitiated = c.ID, c.Generation, true
			in.PayloadRef = payload // sealed at rest by the store
			in.State = state.InboxReceived
		}
		out = append(out, in)
	}
	// Bad items: recorded IGNORED under their msgid (dedupe-safe, never
	// delivered) so the cursor can advance past them; without a msgid they
	// are only alerted, since retrying the same page would loop forever.
	for _, b := range page.Bad {
		o.alert("wecom_sync_message_quarantined", map[string]any{"msgid": b.MsgID, "index": b.Index, "error": b.Err})
		if b.MsgID == "" {
			continue
		}
		out = append(out, state.InboxMessage{ScopeID: o.Scope, BindingID: o.BindingID, ExternalMsgID: b.MsgID, Type: "unparseable", State: state.InboxIgnored, ErrorCategory: CategoryUnparseable, ReceivedAt: now, UpdatedAt: now})
	}
	return out, nil
}

func (o SyncOptions) alert(event string, f map[string]any) {
	if o.Logger != nil {
		f["scope"] = o.Scope
		o.Logger.Log(event, f)
	}
}

// syncPayload extracts the durable inbox payload for one sync_msg item.
// Text keeps the plain body; image/file/voice store JSON media metadata
// (media_id + optional basename-safe file_name). Other types leave payload empty.
func syncPayload(m SyncMessage) string {
	switch strings.ToLower(strings.TrimSpace(m.MsgType)) {
	case "text":
		if m.Text != nil && m.Text.Content != "" {
			return m.Text.Content
		}
		return m.Content
	case "image", "voice", "file":
		mid, fname := m.NestedMedia()
		if mid == "" {
			return ""
		}
		return encodeMediaPayload(mid, fname)
	default:
		return ""
	}
}

// encodeMediaPayload mirrors bridge.EncodeMediaPayload JSON shape so delivery
// can DecodeMediaPayload without importing bridge from wecom (no import cycle).
func encodeMediaPayload(mediaID, fileName string) string {
	type payload struct {
		MediaID  string `json:"media_id"`
		FileName string `json:"file_name,omitempty"`
	}
	p := payload{MediaID: strings.TrimSpace(mediaID)}
	if strings.TrimSpace(fileName) != "" {
		p.FileName = safeFileName(fileName)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	name = strings.ReplaceAll(name, "\\", "/")
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "file"
	}
	return base
}

// SyncAll follows integer has_more, not page length. It resumes from the
// durable cursor, commits each page with its cursor atomically, stops on a
// non-advancing cursor, and on reaching MaxPages records an alert and returns
// a resumable truncated result instead of failing.
func (c *Client) SyncAll(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	if opts.Store == nil {
		return SyncResult{}, fmt.Errorf("wecom: nil sync store")
	}
	if opts.Scope == "" || opts.BindingID == "" || opts.EnterpriseID == "" {
		return SyncResult{}, fmt.Errorf("%w: scope, enterprise and binding required", ErrInvalidArgument)
	}
	if len(opts.Origins.CustomerOrigins) == 0 {
		return SyncResult{}, ErrOriginPolicy
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	max := opts.MaxPages
	if max <= 0 {
		max = 100
	}
	req := opts.Request
	cursor := req.Cursor
	if cursor == "" {
		sc, err := opts.Store.Scope(ctx, opts.Scope)
		if err != nil {
			return SyncResult{}, err
		}
		cursor = sc.Cursor
	}
	result := SyncResult{Scope: opts.Scope, Cursor: cursor}
	for pageNo := 0; pageNo < max; pageNo++ {
		req.Cursor = cursor
		page, err := c.SyncMsg(ctx, opts.AccessToken, req)
		if err != nil {
			return result, err
		}
		if page.HasMore == 1 && (page.NextCursor == "" || page.NextCursor == cursor) {
			_ = opts.Store.SetSyncState(ctx, opts.Scope, ScopeStalled)
			opts.alert("wecom_sync_cursor_stalled", map[string]any{"page": pageNo})
			return result, ErrCursorNoProgress
		}
		msgs, err := opts.classify(ctx, page, now())
		if err != nil {
			if errors.Is(err, ErrMissingMsgID) {
				_ = opts.Store.SetSyncState(ctx, opts.Scope, ScopePausedNoMsgID)
				opts.alert("wecom_sync_missing_msgid", map[string]any{"page": pageNo})
			}
			return result, err
		}
		n, err := opts.Store.CommitSyncPage(ctx, opts.Scope, page.NextCursor, page.HasMore == 1, msgs)
		if err != nil {
			return result, err
		}
		count := len(page.Messages) + len(page.Bad)
		// Idle/empty pages (periodic sync with nothing new) would flood INFO
		// disks; only accept-path pages with work are logged (#13).
		if count > 0 || n > 0 {
			opts.alert("wecom_sync_page_ok", map[string]any{
				"binding_id": opts.BindingID,
				"count":      count,
				"has_more":   page.HasMore == 1,
				"page":       pageNo,
				"inserted":   n,
				"errcode":    page.ErrCode,
			})
		}
		result.Pages++
		result.Messages += count
		result.Inserted += n
		cursor = page.NextCursor
		result.Cursor = cursor
		result.HasMore = page.HasMore == 1
		if !result.HasMore {
			_ = opts.Store.SetSyncState(ctx, opts.Scope, ScopeReady)
			return result, nil
		}
	}
	result.Truncated = true
	_ = opts.Store.SetSyncState(ctx, opts.Scope, ScopePageLimit)
	opts.alert("wecom_sync_page_limit", map[string]any{"max_pages": max})
	return result, nil
}
