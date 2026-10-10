package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Admin console persistence (docs/17 §6): official kf account mirror,
// admin operation idempotency, actor-attributed audit, sealed virtual
// credentials and diagnostic marks.

var (
	// ErrIdempotencyMismatch: an Idempotency-Key was reused with different
	// parameters (HTTP 409).
	ErrIdempotencyMismatch = errors.New("state: idempotency key reused with different parameters")
	// ErrOperationInProgress: the same key is still executing.
	ErrOperationInProgress = errors.New("state: operation in progress")
)

const (
	AccountActive  = "ACTIVE"
	AccountUnknown = "UNKNOWN"
	AccountDeleted = "DELETED"
)

type KFAccount struct {
	OpenKfID  string
	Name      string
	Note      string
	URL       string
	Status    string
	Revision  int64
	SyncedAt  time.Time
	UpdatedAt time.Time
}

type AuditEntry struct {
	ID         int64
	Actor      string
	Key        string
	ObjectType string
	ObjectID   string
	Operation  string
	Result     string
	Summary    string
	Revision   int64
	CreatedAt  time.Time
}

func migrateAdmin(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS kf_accounts (open_kfid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1, synced_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS admin_ops (idem_key TEXT PRIMARY KEY, params_hash TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0, location TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS binding_secrets (binding_id TEXT PRIMARY KEY REFERENCES bindings(id), sealed TEXT NOT NULL, revision INTEGER NOT NULL, exported_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS diag_marks (object_type TEXT NOT NULL, object_id TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, PRIMARY KEY(object_type,object_id))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("state migration: %w", err)
		}
	}
	return addColumn(ctx, db, "audit", "actor", "TEXT NOT NULL DEFAULT ''")
}

const accountCols = `open_kfid,name,note,url,status,revision,synced_at,updated_at`

func scanAccount(row interface{ Scan(...any) error }) (KFAccount, error) {
	var a KFAccount
	var sa, ua int64
	err := row.Scan(&a.OpenKfID, &a.Name, &a.Note, &a.URL, &a.Status, &a.Revision, &sa, &ua)
	a.SyncedAt, a.UpdatedAt = timeFrom(sa), timeFrom(ua)
	return a, err
}

// UpsertAccount records an account as returned by the official API (or an
// UNKNOWN placeholder). Local notes are kept; name/url/status follow the
// official source and bump the revision.
func (s *Store) UpsertAccount(ctx context.Context, a KFAccount) error {
	if a.OpenKfID == "" || a.Status == "" {
		return ErrInvalidID
	}
	now := unix(s.now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO kf_accounts(open_kfid,name,url,status,synced_at,updated_at) VALUES(?,?,?,?,?,?)
	 ON CONFLICT(open_kfid) DO UPDATE SET name=excluded.name,url=CASE WHEN excluded.url<>'' THEN excluded.url ELSE kf_accounts.url END,status=excluded.status,synced_at=excluded.synced_at,revision=kf_accounts.revision+1,updated_at=excluded.updated_at`,
		a.OpenKfID, a.Name, a.URL, a.Status, unix(a.SyncedAt), now)
	return err
}

func (s *Store) Account(ctx context.Context, id string) (KFAccount, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM kf_accounts WHERE open_kfid=?`, id))
	return a, mapNotFound(err)
}

// Accounts pages accounts, optionally filtered by a name/open_kfid
// substring. The filter is a bound parameter, never SQL.
func (s *Store) Accounts(ctx context.Context, q string, offset, limit int) ([]KFAccount, int, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM kf_accounts WHERE name LIKE ? ESCAPE '\' OR open_kfid LIKE ? ESCAPE '\'`, like, like).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM kf_accounts WHERE name LIKE ? ESCAPE '\' OR open_kfid LIKE ? ESCAPE '\' ORDER BY open_kfid LIMIT ? OFFSET ?`, like, like, limit, offset)
	out, err := collect(rows, err, func(r *sql.Rows) (KFAccount, error) { return scanAccount(r) })
	return out, total, err
}

// UpdateAccountLocal applies fn to the account if its revision is still
// expected (compare-and-set); the revision then increments.
func (s *Store) UpdateAccountLocal(ctx context.Context, id string, expected int64, fn func(*KFAccount)) (KFAccount, error) {
	a, err := s.Account(ctx, id)
	if err != nil {
		return a, err
	}
	if a.Revision != expected {
		return a, ErrConflict
	}
	fn(&a)
	r, err := s.db.ExecContext(ctx, `UPDATE kf_accounts SET name=?,note=?,url=?,status=?,revision=revision+1,updated_at=? WHERE open_kfid=? AND revision=?`, a.Name, a.Note, a.URL, a.Status, unix(s.now()), id, expected)
	if err != nil {
		return a, err
	}
	if err = casApplied(r); err != nil {
		return a, err
	}
	return s.Account(ctx, id)
}

// Operation is the stored result of an idempotent admin write.
type Operation struct {
	Done     bool
	Location string
}

// BeginOperation reserves an Idempotency-Key. It returns (nil, nil) when
// the caller should execute; an earlier completed operation with the same
// parameters is returned for replay. Different parameters are refused.
func (s *Store) BeginOperation(ctx context.Context, key, paramsHash string) (*Operation, error) {
	if key == "" || paramsHash == "" {
		return nil, ErrInvalidID
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO admin_ops(idem_key,params_hash,created_at) VALUES(?,?,?) ON CONFLICT(idem_key) DO NOTHING`, key, paramsHash, unix(s.now()))
	if err != nil {
		return nil, err
	}
	if casApplied(r) == nil {
		return nil, nil
	}
	var h, loc string
	var done int
	if err := s.db.QueryRowContext(ctx, `SELECT params_hash,done,location FROM admin_ops WHERE idem_key=?`, key).Scan(&h, &done, &loc); err != nil {
		return nil, err
	}
	if h != paramsHash {
		return nil, ErrIdempotencyMismatch
	}
	if done == 0 {
		return nil, ErrOperationInProgress
	}
	return &Operation{Done: true, Location: loc}, nil
}

// FinishOperation stores the replayable result (redirect location).
func (s *Store) FinishOperation(ctx context.Context, key, location string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE admin_ops SET done=1,location=? WHERE idem_key=? AND done=0`, location, key)
	if err != nil {
		return err
	}
	return casApplied(r)
}

// AddActorAudit writes an attributed audit row (docs/17 §3.7).
func (s *Store) AddActorAudit(ctx context.Context, e AuditEntry) error {
	if e.Key == "" {
		return ErrInvalidID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit(idempotency_key,object_type,object_id,operation,result,parameter_summary,revision,created_at,actor) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, e.Key, e.ObjectType, e.ObjectID, e.Operation, e.Result, e.Summary, e.Revision, unix(s.now()), e.Actor)
	return err
}

func (s *Store) Audits(ctx context.Context, offset, limit int) ([]AuditEntry, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM audit`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,actor,idempotency_key,object_type,object_id,operation,result,parameter_summary,revision,created_at FROM audit ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	out, err := collect(rows, err, func(r *sql.Rows) (AuditEntry, error) {
		var e AuditEntry
		var at int64
		err := r.Scan(&e.ID, &e.Actor, &e.Key, &e.ObjectType, &e.ObjectID, &e.Operation, &e.Result, &e.Summary, &e.Revision, &at)
		e.CreatedAt = timeFrom(at)
		return e, err
	})
	return out, total, err
}

// UpdateBinding changes mutable binding fields under compare-and-set on
// the revision, which then increments: every outbox admitted under the old
// revision is fenced (#25) and tokens of the old revision stop working.
func (s *Store) UpdateBinding(ctx context.Context, id string, expected int64, fn func(*Binding)) (Binding, error) {
	defer s.lock("binding:" + id)()
	b, err := s.Binding(ctx, id)
	if err != nil {
		return b, err
	}
	if b.Revision != expected {
		return b, ErrConflict
	}
	fn(&b)
	active := 0
	if b.Active {
		active = 1
	}
	r, err := s.db.ExecContext(ctx, `UPDATE bindings SET project_id=?,callback_url=?,active=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, b.ProjectID, b.CallbackURL, active, unix(s.now()), id, expected)
	if err != nil {
		return b, err
	}
	if err = casApplied(r); err != nil {
		return b, err
	}
	return s.Binding(ctx, id)
}

// RotateBindingCustomers gives every customer of a binding a new
// generation and UID in one transaction (rebind, docs/17 §3.3): old UIDs
// become stale and can never send again.
//
// It holds the binding lock, the same lock SendGuarded takes around every
// outbound chunk, so a rotation can never complete between a send's
// precondition check and the outbound call (#30).
func (s *Store) RotateBindingCustomers(ctx context.Context, bindingID string) (int, error) {
	defer s.lock("binding:" + bindingID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type cg struct {
		id  string
		gen int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,generation FROM customers WHERE binding_id=?`, bindingID)
	list, err := collect(rows, err, func(r *sql.Rows) (cg, error) {
		var c cg
		return c, r.Scan(&c.id, &c.gen)
	})
	if err != nil {
		return 0, err
	}
	now := unix(s.now())
	for _, c := range list {
		base, err := s.uid()
		if err != nil {
			return 0, err
		}
		uid := generationUID(base, c.gen+1)
		if _, err := tx.ExecContext(ctx, `UPDATE customers SET generation=?,uid=?,revision=revision+1,updated_at=? WHERE id=?`, c.gen+1, uid, now, c.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO customer_uids(binding_id,uid,customer_id,generation,created_at) VALUES(?,?,?,?,?)`, bindingID, uid, c.id, c.gen+1, now); err != nil {
			return 0, err
		}
	}
	return len(list), tx.Commit()
}

// SaveBindingSecret stores generated virtual credentials sealed; they are
// exportable exactly once.
func (s *Store) SaveBindingSecret(ctx context.Context, bindingID, plain string, revision int64) error {
	sealed, err := s.sealField(plain, "binding.secret:"+bindingID)
	if err != nil {
		return err
	}
	now := unix(s.now())
	_, err = s.db.ExecContext(ctx, `INSERT INTO binding_secrets(binding_id,sealed,revision,exported_at,updated_at) VALUES(?,?,?,0,?) ON CONFLICT(binding_id) DO UPDATE SET sealed=excluded.sealed,revision=excluded.revision,exported_at=0,updated_at=excluded.updated_at`, bindingID, sealed, revision, now)
	return err
}

// BindingSecret returns the sealed credentials and whether they were
// already exported.
func (s *Store) BindingSecret(ctx context.Context, bindingID string) (string, bool, error) {
	var sealed string
	var exp int64
	if err := s.db.QueryRowContext(ctx, `SELECT sealed,exported_at FROM binding_secrets WHERE binding_id=?`, bindingID).Scan(&sealed, &exp); err != nil {
		return "", false, mapNotFound(err)
	}
	plain, err := s.openField(sealed, "binding.secret:"+bindingID)
	return plain, exp != 0, err
}

// ExportBindingSecret returns the credentials once; later calls fail with
// ErrConflict until the next rotation.
func (s *Store) ExportBindingSecret(ctx context.Context, bindingID string) (string, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE binding_secrets SET exported_at=? WHERE binding_id=? AND exported_at=0`, unix(s.now()), bindingID)
	if err != nil {
		return "", err
	}
	if err := casApplied(r); err != nil {
		return "", err
	}
	plain, _, err := s.BindingSecret(ctx, bindingID)
	return plain, err
}

// MarkDiagnostic records that an operator verified/closed a diagnostic
// item. It never changes the message state (no blind resend).
func (s *Store) MarkDiagnostic(ctx context.Context, objType, objID, note string) error {
	if objType == "" || objID == "" {
		return ErrInvalidID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO diag_marks(object_type,object_id,note,created_at) VALUES(?,?,?,?) ON CONFLICT(object_type,object_id) DO UPDATE SET note=excluded.note,created_at=excluded.created_at`, objType, objID, note, unix(s.now()))
	return err
}

func (s *Store) DiagnosticMarks(ctx context.Context, objType string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_id,note FROM diag_marks WHERE object_type=?`, objType)
	pairs, err := collect(rows, err, func(r *sql.Rows) ([2]string, error) {
		var p [2]string
		return p, r.Scan(&p[0], &p[1])
	})
	out := map[string]string{}
	for _, p := range pairs {
		out[p[0]] = p[1]
	}
	return out, err
}

// collect scans every row with scan and closes rows.
func collect[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func toArgs(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// InboxByStates lists inbox rows (newest first). Payloads are decrypted by
// the scanner; callers must not render them without step-up.
func (s *Store) InboxByStates(ctx context.Context, customerID string, states []string, limit int) ([]InboxMessage, error) {
	q := inboxSelect() + ` WHERE (?='' OR customer_id=?)`
	args := []any{customerID, customerID}
	if len(states) > 0 {
		q += ` AND state IN (` + placeholders(len(states)) + `)`
		args = append(args, toArgs(states)...)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	return collect(rows, err, func(r *sql.Rows) (InboxMessage, error) { return s.openInbox(scanInbox(r)) })
}

func (s *Store) OutboxByStates(ctx context.Context, customerID string, states []string, limit int) ([]OutboxMessage, error) {
	q := outboxSelect() + ` WHERE (?='' OR customer_id=?)`
	args := []any{customerID, customerID}
	if len(states) > 0 {
		q += ` AND state IN (` + placeholders(len(states)) + `)`
		args = append(args, toArgs(states)...)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC LIMIT ?`, append(args, limit)...)
	return collect(rows, err, func(r *sql.Rows) (OutboxMessage, error) { return s.openOutbox(scanOutbox(r)) })
}

// Customers finds customers by exact id/UID/external_userid, or lists the
// most recent ones when q is empty.
func (s *Store) Customers(ctx context.Context, q string, limit int) ([]Customer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+customerCols+` FROM customers WHERE ?='' OR id=? OR uid=? OR external_user_id=? ORDER BY updated_at DESC LIMIT ?`, q, q, q, q, limit)
	return collect(rows, err, func(r *sql.Rows) (Customer, error) { return scanCustomer(r) })
}

// Bindings lists all bindings.
func (s *Store) Bindings(ctx context.Context) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,enterprise_id,open_kfid,project_id,virtual_token_hash,callback_url,active,revision,created_at,updated_at FROM bindings ORDER BY id`)
	return collect(rows, err, func(r *sql.Rows) (Binding, error) { return scanBinding(r) })
}

// Overview counts for the dashboard.
func (s *Store) Overview(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for k, q := range map[string]string{
		"bindings_active":  `SELECT COUNT(1) FROM bindings WHERE active=1`,
		"customers_paused": `SELECT COUNT(1) FROM customers WHERE state<>'AI_ELIGIBLE'`,
		"outbox_unknown":   `SELECT COUNT(1) FROM outbox WHERE state='UNKNOWN'`,
		"inbox_unknown":    `SELECT COUNT(1) FROM inbox WHERE state='DELIVERY_UNKNOWN'`,
		"inbox_retry":      `SELECT COUNT(1) FROM inbox WHERE state='RETRY_WAIT'`,
		"inbox_held":       `SELECT COUNT(1) FROM inbox WHERE state='HELD'`,
		"scopes_pending":   `SELECT COUNT(1) FROM sync_scopes WHERE pending=1`,
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}
