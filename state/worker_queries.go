package state

import "context"

// PendingScopes lists scopes whose pending flag is set (to resume after a
// crash or a page limit), oldest update first.
func (s *Store) PendingScopes(ctx context.Context, limit int) ([]SyncScope, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,binding_id,cursor,pending,last_success_at,next_attempt_at,state,updated_at FROM sync_scopes WHERE pending=1 ORDER BY updated_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncScope
	for rows.Next() {
		x, e := scanScope(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// InboxForDelivery returns customer messages awaiting delivery to
// cc-connect (RECEIVED, CLASSIFIED, READY, RETRY_WAIT under maxAttempts),
// in arrival order. IGNORED/UNSUPPORTED/HELD/UNKNOWN rows are never returned.
func (s *Store) InboxForDelivery(ctx context.Context, maxAttempts, limit int) ([]InboxMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, inboxSelect()+` WHERE customer_id<>'' AND (state IN ('RECEIVED','CLASSIFIED','READY') OR (state='RETRY_WAIT' AND attempt<?)) ORDER BY id LIMIT ?`, maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var raw []InboxMessage
	for rows.Next() {
		m, e := scanInbox(rows)
		if e != nil {
			return nil, e
		}
		raw = append(raw, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	out := make([]InboxMessage, 0, len(raw))
	for _, m := range raw {
		m, err = s.openInbox(m, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// RecoverInFlight runs at startup: work interrupted mid-request is marked
// for review instead of being blindly retried (docs/07 §6, docs/09 §2).
func (s *Store) RecoverInFlight(ctx context.Context) (inbox, outbox int64, err error) {
	now := unix(s.now())
	r, err := s.db.ExecContext(ctx, `UPDATE inbox SET state=?,error_category='interrupted',updated_at=? WHERE state=?`, InboxDeliveryUnknown, now, InboxPosting)
	if err != nil {
		return 0, 0, err
	}
	inbox, _ = r.RowsAffected()
	r, err = s.db.ExecContext(ctx, `UPDATE outbox SET state=?,error_category='interrupted',updated_at=? WHERE state=?`, OutboxUnknown, now, OutboxSending)
	if err != nil {
		return inbox, 0, err
	}
	outbox, _ = r.RowsAffected()
	return inbox, outbox, nil
}
