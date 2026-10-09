package wecom

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

var ErrCursorNoProgress = errors.New("wecom: sync cursor made no progress")

// SyncPageStore is intentionally broad for compatibility with older callers.
// Implementations may expose CommitSyncPage(ctx, scope, SyncResponse), while
// state.Store uses CommitSyncPage(ctx, scope, cursor, hasMore, []InboxMessage).
type SyncPageStore interface{}
type syncPageStoreV1 interface {
	CommitSyncPage(context.Context, string, SyncResponse) error
}
type syncPageStoreState interface {
	CommitSyncPage(context.Context, string, string, bool, []state.InboxMessage) (int, error)
}
type SyncOptions struct {
	Scope       string
	BindingID   string
	AccessToken string
	Request     SyncRequest
	Store       SyncPageStore
	MaxPages    int
}
type SyncResult struct {
	Scope    string
	Pages    int
	Messages int
	Cursor   string
	HasMore  bool
}

func stableCompatID(msgID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(msgID))
	v := int64(h.Sum64() & 0x7fffffffffffffff)
	if v == 0 {
		v = 1
	}
	return v
}
func stateMessages(scope, bindingID string, page SyncResponse) []state.InboxMessage {
	now := time.Now()
	out := make([]state.InboxMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		ct := time.Time{}
		if m.SendTime > 0 {
			ct = time.Unix(m.SendTime, 0)
		}
		payload := m.Content
		if payload == "" && m.Text != nil {
			payload = m.Text.Content
		}
		out = append(out, state.InboxMessage{ScopeID: scope, BindingID: bindingID, ExternalMsgID: m.MsgID, CompatMsgID: stableCompatID(m.MsgID), CreateTime: ct, Source: fmt.Sprintf("origin:%d", m.Origin), Type: m.MsgType, PayloadRef: payload, ReceivedAt: now, UpdatedAt: now})
	}
	return out
}

// SyncAll follows integer has_more, not page length. A repeated cursor is a
// hard stop to avoid spinning forever on a broken upstream.
func (c *Client) SyncAll(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	if opts.Store == nil {
		return SyncResult{}, fmt.Errorf("wecom: nil sync store")
	}
	if opts.Scope == "" {
		return SyncResult{}, fmt.Errorf("wecom: empty sync scope")
	}
	max := opts.MaxPages
	if max <= 0 {
		max = 1000
	}
	req := opts.Request
	cursor := req.Cursor
	result := SyncResult{Scope: opts.Scope, Cursor: cursor}
	for pageNo := 0; pageNo < max; pageNo++ {
		req.Cursor = cursor
		page, err := c.SyncMsg(ctx, opts.AccessToken, req)
		if err != nil {
			return result, err
		}
		if pageNo >= 0 && page.HasMore == 1 && page.NextCursor == cursor {
			return result, ErrCursorNoProgress
		}
		var commitErr error
		switch st := opts.Store.(type) {
		case syncPageStoreV1:
			commitErr = st.CommitSyncPage(ctx, opts.Scope, page)
		case syncPageStoreState:
			msgs := stateMessages(opts.Scope, opts.BindingID, page)
			for i := range msgs {
				if msgs[i].BindingID == "" {
					msgs[i].BindingID = opts.Scope
				}
				if msgs[i].ExternalMsgID == "" {
					msgs[i].ExternalMsgID = fmt.Sprintf("page-%d-%d", pageNo, i)
				}
				if msgs[i].CompatMsgID <= 0 {
					msgs[i].CompatMsgID = int64(result.Messages + i + 1)
				}
			}
			_, commitErr = st.CommitSyncPage(ctx, opts.Scope, page.NextCursor, page.HasMore == 1, msgs)
		default:
			return result, fmt.Errorf("wecom: unsupported sync store %T", opts.Store)
		}
		if commitErr != nil {
			return result, commitErr
		}
		result.Pages++
		result.Messages += len(page.Messages)
		cursor = page.NextCursor
		result.Cursor = cursor
		result.HasMore = page.HasMore == 1
		if page.HasMore == 0 {
			return result, nil
		}
		if cursor == "" {
			return result, ErrCursorNoProgress
		}
	}
	return result, fmt.Errorf("wecom: sync page limit %d exceeded", max)
}
