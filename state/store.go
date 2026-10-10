// Package state implements the durable state machine for the WeCom customer
// gateway.  The store deliberately keeps network operations outside of SQL
// transactions: callers first reserve a state transition, perform the
// external request, and then record the outcome.
package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound        = errors.New("state: not found")
	ErrConflict        = errors.New("state: revision conflict")
	ErrStaleGeneration = errors.New("state: stale customer generation")
	ErrHeld            = errors.New("state: customer is held")
	ErrNoProgress      = errors.New("state: cursor made no progress")
	ErrInvalidState    = errors.New("state: invalid transition")
	ErrUnknownResult   = errors.New("state: delivery result is unknown")
	ErrInvalidID       = errors.New("state: invalid identifier")
)

// Delivery and handover states are strings in SQLite intentionally.  This
// keeps old records readable when a new state is introduced.
const (
	CustomerAIEligible   = "AI_ELIGIBLE"
	CustomerWaitingHuman = "WAITING_HUMAN"
	CustomerHuman        = "HUMAN"
	CustomerClosed       = "CLOSED"
	CustomerUnknown      = "UNKNOWN"

	InboxReceived        = "RECEIVED"
	InboxClassified      = "CLASSIFIED"
	InboxReady           = "READY"
	InboxPosting         = "POSTING"
	InboxHTTPAccepted    = "HTTP_ACCEPTED"
	InboxRetryWait       = "RETRY_WAIT"
	InboxDeliveryUnknown = "DELIVERY_UNKNOWN"
	InboxHeld            = "HELD"
	InboxExpired         = "EXPIRED"
	InboxIgnored         = "IGNORED"
	InboxUnsupported     = "UNSUPPORTED"

	OutboxCreated          = "CREATED"
	OutboxValidated        = "VALIDATED"
	OutboxSending          = "SENDING"
	OutboxUpstreamAccepted = "UPSTREAM_ACCEPTED"
	OutboxDeliveryFailed   = "DELIVERY_FAILED"
	OutboxUnknown          = "UNKNOWN"
	OutboxRejected         = "REJECTED"
	OutboxBlocked          = "BLOCKED"
)

var safeUID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Enterprise stores only a reference to a credential. Secret values should be
// supplied by an external secret manager or encrypted config, not this table.
type Enterprise struct {
	ID            string
	TenantID      string
	CorpID        string
	CredentialRef string
	Status        string
	Revision      int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Binding struct {
	ID               string
	EnterpriseID     string
	OpenKfID         string
	ProjectID        string
	VirtualTokenHash string
	CallbackURL      string
	Active           bool
	Revision         int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Customer struct {
	ID                string
	EnterpriseID      string
	BindingID         string
	ExternalUserID    string
	Generation        int64
	UID               string
	Nickname          string
	NicknameUpdatedAt time.Time
	OfficialStatus    string
	State             string
	FenceGeneration   int64
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type SyncScope struct {
	ID            string
	BindingID     string
	Cursor        string
	Pending       bool
	LastSuccessAt time.Time
	NextAttemptAt time.Time
	State         string
	UpdatedAt     time.Time
}

type InboxMessage struct {
	ID            int64
	ScopeID       string
	BindingID     string
	CustomerID    string
	Generation    int64
	ExternalMsgID string
	CompatMsgID   int64
	CreateTime    time.Time
	Source        string
	Type          string
	PayloadRef    string
	State         string
	ErrorCategory string
	Attempt       int
	ReceivedAt    time.Time
	UpdatedAt     time.Time
}

type OutboxMessage struct {
	ID             string
	BindingID      string
	CustomerID     string
	Generation     int64
	UID            string
	Body           string
	ContentRef     string
	State          string
	BudgetUnits    int
	BudgetReserved bool
	Attempt        int
	ErrorCategory  string
	ExternalMsgID  string
	InFlightAt     time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store owns one SQLite handle. database/sql itself supplies a pool, while
// locks only protect state decisions for one scope/customer. No network call
// should be made while a lock is held.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	uid   func() (string, error)
	locks keyedLocks
}

type Options struct {
	Now          func() time.Time
	UIDGenerator func() (string, error)
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s, err := New(db, Options{})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// New initializes an already opened database. It is useful for tests and for
// embedding the state layer in a service that manages the SQL handle.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("state: nil database")
	}
	s := &Store{db: db, now: opts.Now, uid: opts.UIDGenerator}
	if s.now == nil {
		s.now = time.Now
	}
	if s.uid == nil {
		s.uid = randomAlias
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("state: %s: %w", q, err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS enterprises (
	 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, corp_id TEXT NOT NULL,
	 credential_ref TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'ACTIVE',
	 revision INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	 UNIQUE(tenant_id), UNIQUE(corp_id))`,
		`CREATE TABLE IF NOT EXISTS bindings (
	 id TEXT PRIMARY KEY, enterprise_id TEXT NOT NULL REFERENCES enterprises(id), open_kfid TEXT NOT NULL,
	 project_id TEXT NOT NULL, virtual_token_hash TEXT NOT NULL DEFAULT '', callback_url TEXT NOT NULL DEFAULT '',
	 active INTEGER NOT NULL DEFAULT 1, revision INTEGER NOT NULL DEFAULT 1,
	 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	 UNIQUE(enterprise_id,open_kfid))`,
		`CREATE TABLE IF NOT EXISTS customers (
	 id TEXT PRIMARY KEY, enterprise_id TEXT NOT NULL REFERENCES enterprises(id), binding_id TEXT NOT NULL REFERENCES bindings(id),
	 external_user_id TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 1, uid TEXT NOT NULL,
	 nickname TEXT NOT NULL DEFAULT '', nickname_updated_at INTEGER NOT NULL DEFAULT 0,
	 official_status TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'AI_ELIGIBLE',
	 fence_generation INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1,
	 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	 UNIQUE(enterprise_id,external_user_id,binding_id), UNIQUE(binding_id,uid))`,
		`CREATE TABLE IF NOT EXISTS sync_scopes (
	 id TEXT PRIMARY KEY, binding_id TEXT NOT NULL REFERENCES bindings(id), cursor TEXT NOT NULL DEFAULT '',
	 pending INTEGER NOT NULL DEFAULT 0, last_success_at INTEGER NOT NULL DEFAULT 0,
	 next_attempt_at INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'READY', updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS inbox (
	 id INTEGER PRIMARY KEY AUTOINCREMENT, scope_id TEXT NOT NULL REFERENCES sync_scopes(id), binding_id TEXT NOT NULL REFERENCES bindings(id),
	 customer_id TEXT NOT NULL DEFAULT '', generation INTEGER NOT NULL DEFAULT 0, external_msg_id TEXT NOT NULL,
	 compat_msg_id INTEGER NOT NULL CHECK(compat_msg_id > 0), create_time INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT '',
	 payload_ref TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'RECEIVED', error_category TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 0,
	 received_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	 UNIQUE(scope_id,external_msg_id), UNIQUE(binding_id,compat_msg_id))`,
		`CREATE TABLE IF NOT EXISTS outbox (
	 id TEXT PRIMARY KEY, binding_id TEXT NOT NULL REFERENCES bindings(id), customer_id TEXT NOT NULL REFERENCES customers(id),
	 generation INTEGER NOT NULL, uid TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', content_ref TEXT NOT NULL DEFAULT '',
	 state TEXT NOT NULL DEFAULT 'CREATED', budget_units INTEGER NOT NULL DEFAULT 1, budget_reserved INTEGER NOT NULL DEFAULT 0,
	 attempt INTEGER NOT NULL DEFAULT 0, error_category TEXT NOT NULL DEFAULT '', external_msg_id TEXT NOT NULL DEFAULT '',
	 in_flight_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS handover_events (
	 id INTEGER PRIMARY KEY AUTOINCREMENT, customer_id TEXT NOT NULL REFERENCES customers(id), old_generation INTEGER NOT NULL,
	 state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit (
	 id INTEGER PRIMARY KEY AUTOINCREMENT, idempotency_key TEXT NOT NULL UNIQUE, object_type TEXT NOT NULL, object_id TEXT NOT NULL,
	 operation TEXT NOT NULL, result TEXT NOT NULL, parameter_summary TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_customers_uid ON customers(binding_id,uid)`,
		`CREATE INDEX IF NOT EXISTS idx_inbox_state ON inbox(binding_id,state)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_state ON outbox(binding_id,state)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("state migration: %w", err)
		}
	}
	return nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
func timeFrom(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}
func nullString(s string) any { return s }

func randomAlias() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return "bcu_" + hex.EncodeToString(b[:]), nil
}
func randomID(prefix string) (string, error) {
	a, err := randomAlias()
	if err != nil {
		return "", err
	}
	return prefix + a, nil
}
func validUID(u string) bool {
	return u != "" && len(u) <= 200 && safeUID.MatchString(u) && !strings.Contains(u, "@all")
}
func generationUID(alias string, generation int64) string {
	base := regexp.MustCompile(`_g[0-9]+$`).ReplaceAllString(alias, "")
	return fmt.Sprintf("%s_g%d", base, generation)
}

// Lock serializes only the supplied key. It is intentionally local and short;
// use it around a transaction that decides state, not around HTTP/AI work.
func (s *Store) lock(key string) func() { return s.locks.acquire(key) }
func (s *Store) WithLock(key string, fn func() error) error {
	if key == "" {
		return ErrInvalidID
	}
	defer s.lock(key)()
	return fn()
}
func (s *Store) WithCustomerLock(customerID string, fn func() error) error {
	return s.WithLock("customer:"+customerID, fn)
}
func (s *Store) WithScopeLock(scopeID string, fn func() error) error {
	return s.WithLock("scope:"+scopeID, fn)
}

func (s *Store) PutEnterprise(ctx context.Context, e Enterprise) error {
	if e.ID == "" || e.TenantID == "" || e.CorpID == "" || e.CredentialRef == "" {
		return ErrInvalidID
	}
	n := s.now()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = n
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = n
	}
	if e.Revision <= 0 {
		e.Revision = 1
	}
	if e.Status == "" {
		e.Status = "ACTIVE"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO enterprises(id,tenant_id,corp_id,credential_ref,status,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
	 ON CONFLICT(id) DO UPDATE SET tenant_id=excluded.tenant_id,corp_id=excluded.corp_id,credential_ref=excluded.credential_ref,status=excluded.status,revision=excluded.revision,updated_at=excluded.updated_at`, e.ID, e.TenantID, e.CorpID, e.CredentialRef, e.Status, e.Revision, unix(e.CreatedAt), unix(e.UpdatedAt))
	return err
}
func scanEnterprise(row interface{ Scan(...any) error }) (Enterprise, error) {
	var e Enterprise
	var ca, ua int64
	err := row.Scan(&e.ID, &e.TenantID, &e.CorpID, &e.CredentialRef, &e.Status, &e.Revision, &ca, &ua)
	e.CreatedAt = timeFrom(ca)
	e.UpdatedAt = timeFrom(ua)
	return e, err
}
func (s *Store) Enterprise(ctx context.Context, id string) (Enterprise, error) {
	e, err := scanEnterprise(s.db.QueryRowContext(ctx, `SELECT id,tenant_id,corp_id,credential_ref,status,revision,created_at,updated_at FROM enterprises WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}

func (s *Store) PutBinding(ctx context.Context, b Binding) error {
	if b.ID == "" || b.EnterpriseID == "" || b.OpenKfID == "" || b.ProjectID == "" {
		return ErrInvalidID
	}
	n := s.now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = n
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = n
	}
	if b.Revision <= 0 {
		b.Revision = 1
	}
	active := 0
	if b.Active {
		active = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bindings(id,enterprise_id,open_kfid,project_id,virtual_token_hash,callback_url,active,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,virtual_token_hash=excluded.virtual_token_hash,callback_url=excluded.callback_url,active=excluded.active,revision=excluded.revision,updated_at=excluded.updated_at`, b.ID, b.EnterpriseID, b.OpenKfID, b.ProjectID, b.VirtualTokenHash, b.CallbackURL, active, b.Revision, unix(b.CreatedAt), unix(b.UpdatedAt))
	return err
}
func scanBinding(row interface{ Scan(...any) error }) (Binding, error) {
	var b Binding
	var a int
	var cat, uat int64
	err := row.Scan(&b.ID, &b.EnterpriseID, &b.OpenKfID, &b.ProjectID, &b.VirtualTokenHash, &b.CallbackURL, &a, &b.Revision, &cat, &uat)
	b.Active = a != 0
	b.CreatedAt = timeFrom(cat)
	b.UpdatedAt = timeFrom(uat)
	return b, err
}
func (s *Store) Binding(ctx context.Context, id string) (Binding, error) {
	b, err := scanBinding(s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,open_kfid,project_id,virtual_token_hash,callback_url,active,revision,created_at,updated_at FROM bindings WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}
func (s *Store) BindingByOpenKfID(ctx context.Context, enterpriseID, open string) (Binding, error) {
	b, err := scanBinding(s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,open_kfid,project_id,virtual_token_hash,callback_url,active,revision,created_at,updated_at FROM bindings WHERE enterprise_id=? AND open_kfid=?`, enterpriseID, open))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (s *Store) EnsureScope(ctx context.Context, scopeID, bindingID string) error {
	if scopeID == "" || bindingID == "" {
		return ErrInvalidID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_scopes(id,binding_id,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`, scopeID, bindingID, unix(s.now()))
	return err
}
func scanScope(row interface{ Scan(...any) error }) (SyncScope, error) {
	var x SyncScope
	var p int
	var ls, na, u int64
	err := row.Scan(&x.ID, &x.BindingID, &x.Cursor, &p, &ls, &na, &x.State, &u)
	x.Pending = p != 0
	x.LastSuccessAt = timeFrom(ls)
	x.NextAttemptAt = timeFrom(na)
	x.UpdatedAt = timeFrom(u)
	return x, err
}
func (s *Store) Scope(ctx context.Context, id string) (SyncScope, error) {
	x, err := scanScope(s.db.QueryRowContext(ctx, `SELECT id,binding_id,cursor,pending,last_success_at,next_attempt_at,state,updated_at FROM sync_scopes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return x, err
}
func (s *Store) MarkSyncPending(ctx context.Context, id string, pending bool) error {
	if id == "" {
		return ErrInvalidID
	}
	p := 0
	if pending {
		p = 1
	}
	r, err := s.db.ExecContext(ctx, `UPDATE sync_scopes SET pending=?,updated_at=? WHERE id=?`, p, unix(s.now()), id)
	if err != nil {
		return err
	}
	return rowsOrNotFound(r)
}

func (s *Store) EnsureCustomer(ctx context.Context, enterpriseID, bindingID, external string) (Customer, error) {
	if enterpriseID == "" || bindingID == "" || external == "" {
		return Customer{}, ErrInvalidID
	}
	defer s.lock("customer-key:" + enterpriseID + ":" + bindingID + ":" + external)()
	c, err := scanCustomer(s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE enterprise_id=? AND binding_id=? AND external_user_id=?`, enterpriseID, bindingID, external))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	id, err := randomID("cus_")
	if err != nil {
		return c, err
	}
	uid, err := s.uid()
	if err != nil {
		return c, err
	}
	uid = generationUID(uid, 1)
	if !validUID(uid) {
		return c, fmt.Errorf("state: uid generator returned invalid uid")
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO customers(id,enterprise_id,binding_id,external_user_id,generation,uid,state,created_at,updated_at) VALUES(?,?,?,?,1,?,'AI_ELIGIBLE',?,?)`, id, enterpriseID, bindingID, external, uid, unix(now), unix(now))
	if err != nil {
		return c, err
	}
	return s.Customer(ctx, id)
}
func scanCustomer(row interface{ Scan(...any) error }) (Customer, error) {
	var c Customer
	var n, cat, ua int64
	err := row.Scan(&c.ID, &c.EnterpriseID, &c.BindingID, &c.ExternalUserID, &c.Generation, &c.UID, &c.Nickname, &n, &c.OfficialStatus, &c.State, &c.FenceGeneration, &c.Revision, &cat, &ua)
	c.NicknameUpdatedAt = timeFrom(n)
	c.CreatedAt = timeFrom(cat)
	c.UpdatedAt = timeFrom(ua)
	return c, err
}
func (s *Store) Customer(ctx context.Context, id string) (Customer, error) {
	c, err := scanCustomer(s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func (s *Store) CustomerByUID(ctx context.Context, bindingID, uid string) (Customer, error) {
	c, err := scanCustomer(s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE binding_id=? AND uid=?`, bindingID, uid))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func (s *Store) SetNickname(ctx context.Context, id, nickname string, at time.Time) error {
	if at.IsZero() {
		at = s.now()
	}
	r, err := s.db.ExecContext(ctx, `UPDATE customers SET nickname=?,nickname_updated_at=?,revision=revision+1,updated_at=? WHERE id=?`, nickname, unix(at), unix(s.now()), id)
	if err != nil {
		return err
	}
	return rowsOrNotFound(r)
}
func (s *Store) SetOfficialStatus(ctx context.Context, id, status string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE customers SET official_status=?,revision=revision+1,updated_at=? WHERE id=?`, status, unix(s.now()), id)
	if err != nil {
		return err
	}
	return rowsOrNotFound(r)
}
func (s *Store) SetCustomerState(ctx context.Context, id, status string) error {
	if !validCustomerState(status) {
		return ErrInvalidState
	}
	r, err := s.db.ExecContext(ctx, `UPDATE customers SET state=?,revision=revision+1,updated_at=? WHERE id=?`, status, unix(s.now()), id)
	if err != nil {
		return err
	}
	return rowsOrNotFound(r)
}
func validCustomerState(v string) bool {
	switch v {
	case CustomerAIEligible, CustomerWaitingHuman, CustomerHuman, CustomerClosed, CustomerUnknown:
		return true
	}
	return false
}

// BeginHandover first fences the current generation locally. The caller may
// then make the official API request without holding a SQL transaction.
func (s *Store) BeginHandover(ctx context.Context, customerID, reason string) (Customer, error) {
	defer s.lock("customer:" + customerID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Customer{}, err
	}
	defer tx.Rollback()
	c, err := scanCustomer(tx.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE id=?`, customerID))
	if err != nil {
		return Customer{}, mapNotFound(err)
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `UPDATE customers SET state=?,fence_generation=?,revision=revision+1,updated_at=? WHERE id=?`, CustomerWaitingHuman, c.Generation, unix(now), customerID); err != nil {
		return Customer{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handover_events(customer_id,old_generation,state,reason,created_at) VALUES(?,?,?,?,?)`, customerID, c.Generation, CustomerWaitingHuman, reason, unix(now)); err != nil {
		return Customer{}, err
	}
	if err = tx.Commit(); err != nil {
		return Customer{}, err
	}
	c.State = CustomerWaitingHuman
	c.FenceGeneration = c.Generation
	c.Revision++
	c.UpdatedAt = now
	return c, nil
}

// SetHandoverStatus records a known official status. Unknown and query errors
// are intentionally safe: they keep the customer from being AI eligible.
func (s *Store) SetHandoverStatus(ctx context.Context, customerID, status, reason string) error {
	// AI_ELIGIBLE is only reachable through RecoverCustomer, which allocates
	// a new generation and fences every old UID. A status callback cannot
	// silently reopen an old generation.
	if !validCustomerState(status) || status == CustomerAIEligible {
		return ErrInvalidState
	}
	defer s.lock("customer:" + customerID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gen int64
	if err = tx.QueryRowContext(ctx, `SELECT generation FROM customers WHERE id=?`, customerID).Scan(&gen); err != nil {
		return mapNotFound(err)
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `UPDATE customers SET state=?,revision=revision+1,updated_at=? WHERE id=?`, status, unix(now), customerID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handover_events(customer_id,old_generation,state,reason,created_at) VALUES(?,?,?,?,?)`, customerID, gen, status, reason, unix(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverCustomer(ctx context.Context, customerID, reason string) (Customer, error) {
	defer s.lock("customer:" + customerID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Customer{}, err
	}
	defer tx.Rollback()
	c, err := scanCustomer(tx.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE id=?`, customerID))
	if err != nil {
		return Customer{}, mapNotFound(err)
	}
	if c.State != CustomerHuman && c.State != CustomerWaitingHuman {
		return Customer{}, fmt.Errorf("state: cannot recover from %s", c.State)
	}
	uid, err := s.uid()
	if err != nil {
		return Customer{}, err
	}
	uid = generationUID(uid, c.Generation+1)
	if !validUID(uid) {
		return Customer{}, ErrInvalidID
	}
	now := s.now()
	newGen := c.Generation + 1
	if _, err = tx.ExecContext(ctx, `UPDATE customers SET generation=?,uid=?,state=?,fence_generation=?,revision=revision+1,updated_at=? WHERE id=?`, newGen, uid, CustomerAIEligible, newGen, unix(now), customerID); err != nil {
		return Customer{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handover_events(customer_id,old_generation,state,reason,created_at) VALUES(?,?,?,?,?)`, customerID, c.Generation, CustomerAIEligible, reason, unix(now)); err != nil {
		return Customer{}, err
	}
	if err = tx.Commit(); err != nil {
		return Customer{}, err
	}
	c.Generation = newGen
	c.UID = uid
	c.State = CustomerAIEligible
	c.FenceGeneration = newGen
	c.Revision++
	c.UpdatedAt = now
	return c, nil
}

func (s *Store) RotateGeneration(ctx context.Context, customerID, reason string) (Customer, error) {
	return s.RecoverCustomer(ctx, customerID, reason)
}

// CommitSyncPage atomically writes all inbox rows and the next cursor. A page
// may be retried safely; duplicate external message IDs are ignored. If
// hasMore is true, a cursor that does not advance returns ErrNoProgress.
func (s *Store) CommitSyncPage(ctx context.Context, scopeID, nextCursor string, hasMore bool, msgs []InboxMessage) (int, error) {
	defer s.lock("scope:" + scopeID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT cursor FROM sync_scopes WHERE id=?`, scopeID).Scan(&current); err != nil {
		return 0, mapNotFound(err)
	}
	if hasMore && nextCursor == current {
		return 0, ErrNoProgress
	}
	inserted := 0
	now := s.now()
	for _, in := range msgs {
		if in.ScopeID == "" {
			in.ScopeID = scopeID
		}
		if in.ScopeID != scopeID || in.BindingID == "" || in.ExternalMsgID == "" || in.CompatMsgID <= 0 {
			return 0, ErrInvalidID
		}
		if in.State == "" {
			in.State = InboxReceived
		}
		if in.ReceivedAt.IsZero() {
			in.ReceivedAt = now
		}
		if in.UpdatedAt.IsZero() {
			in.UpdatedAt = now
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO inbox(scope_id,binding_id,customer_id,generation,external_msg_id,compat_msg_id,create_time,source,type,payload_ref,state,error_category,attempt,received_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope_id,external_msg_id) DO NOTHING`, scopeID, in.BindingID, in.CustomerID, in.Generation, in.ExternalMsgID, in.CompatMsgID, unix(in.CreateTime), in.Source, in.Type, in.PayloadRef, in.State, in.ErrorCategory, in.Attempt, unix(in.ReceivedAt), unix(in.UpdatedAt))
		if e != nil {
			return 0, e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return 0, e
		}
		inserted += int(n)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sync_scopes SET cursor=?,pending=0,last_success_at=?,updated_at=? WHERE id=?`, nextCursor, unix(now), unix(now), scopeID); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func scanInbox(row interface{ Scan(...any) error }) (InboxMessage, error) {
	var m InboxMessage
	var ct, ra, ua int64
	err := row.Scan(&m.ID, &m.ScopeID, &m.BindingID, &m.CustomerID, &m.Generation, &m.ExternalMsgID, &m.CompatMsgID, &ct, &m.Source, &m.Type, &m.PayloadRef, &m.State, &m.ErrorCategory, &m.Attempt, &ra, &ua)
	m.CreateTime = timeFrom(ct)
	m.ReceivedAt = timeFrom(ra)
	m.UpdatedAt = timeFrom(ua)
	return m, err
}
func inboxSelect() string {
	return `SELECT id,scope_id,binding_id,customer_id,generation,external_msg_id,compat_msg_id,create_time,source,type,payload_ref,state,error_category,attempt,received_at,updated_at FROM inbox`
}
func (s *Store) Inbox(ctx context.Context, id int64) (InboxMessage, error) {
	m, err := scanInbox(s.db.QueryRowContext(ctx, inboxSelect()+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return m, err
}
func (s *Store) InboxByExternalID(ctx context.Context, scopeID, id string) (InboxMessage, error) {
	m, err := scanInbox(s.db.QueryRowContext(ctx, inboxSelect()+` WHERE scope_id=? AND external_msg_id=?`, scopeID, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return m, err
}
func (s *Store) TransitionInbox(ctx context.Context, id int64, to string, errCategory string) (InboxMessage, error) {
	if !validInboxState(to) {
		return InboxMessage{}, ErrInvalidState
	}
	m, err := s.Inbox(ctx, id)
	if err != nil {
		return m, err
	}
	if !allowedInbox(m.State, to) {
		return m, fmt.Errorf("%w: inbox %s -> %s", ErrInvalidState, m.State, to)
	}
	r, e := s.db.ExecContext(ctx, `UPDATE inbox SET state=?,error_category=?,attempt=attempt+1,updated_at=? WHERE id=?`, to, errCategory, unix(s.now()), id)
	if e != nil {
		return m, e
	}
	if e = rowsOrNotFound(r); e != nil {
		return m, e
	}
	return s.Inbox(ctx, id)
}
func validInboxState(v string) bool {
	switch v {
	case InboxReceived, InboxClassified, InboxReady, InboxPosting, InboxHTTPAccepted, InboxRetryWait, InboxDeliveryUnknown, InboxHeld, InboxExpired, InboxIgnored, InboxUnsupported:
		return true
	}
	return false
}
func allowedInbox(a, b string) bool {
	if a == b {
		return true
	}
	switch a {
	case InboxReceived:
		return b == InboxClassified || b == InboxIgnored || b == InboxUnsupported
	case InboxClassified:
		return b == InboxReady || b == InboxIgnored || b == InboxUnsupported
	case InboxReady:
		return b == InboxPosting || b == InboxHeld || b == InboxExpired
	case InboxPosting:
		return b == InboxHTTPAccepted || b == InboxRetryWait || b == InboxDeliveryUnknown
	case InboxRetryWait:
		return b == InboxPosting || b == InboxHeld
	case InboxHTTPAccepted:
		return b == InboxDeliveryUnknown || b == InboxHeld
	}
	return false
}

// CreateOutbox validates the current generation and local handover fence while
// reserving budget in one transaction. The caller must send after commit.
func (s *Store) CreateOutbox(ctx context.Context, o OutboxMessage) (OutboxMessage, error) {
	defer s.lock("customer:" + o.CustomerID)()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxMessage{}, err
	}
	defer tx.Rollback()
	var c Customer
	c, err = scanCustomer(tx.QueryRowContext(ctx, `SELECT id,enterprise_id,binding_id,external_user_id,generation,uid,nickname,nickname_updated_at,official_status,state,fence_generation,revision,created_at,updated_at FROM customers WHERE id=?`, o.CustomerID))
	if err != nil {
		return OutboxMessage{}, mapNotFound(err)
	}
	if c.Generation != o.Generation || c.UID != o.UID {
		return OutboxMessage{}, ErrStaleGeneration
	}
	if c.State != CustomerAIEligible {
		return OutboxMessage{}, ErrHeld
	}
	if o.ID == "" {
		o.ID, err = randomID("out_")
		if err != nil {
			return OutboxMessage{}, err
		}
	}
	if o.State == "" {
		o.State = OutboxCreated
	}
	if o.BudgetUnits <= 0 {
		o.BudgetUnits = 1
	}
	now := s.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox(id,binding_id,customer_id,generation,uid,body,content_ref,state,budget_units,budget_reserved,attempt,error_category,external_msg_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?,?,?)`, o.ID, c.BindingID, c.ID, c.Generation, c.UID, o.Body, o.ContentRef, o.State, o.BudgetUnits, o.Attempt, o.ErrorCategory, o.ExternalMsgID, unix(now), unix(now))
	if err != nil {
		return OutboxMessage{}, err
	}
	if err = tx.Commit(); err != nil {
		return OutboxMessage{}, err
	}
	o.BindingID = c.BindingID
	o.Generation = c.Generation
	o.UID = c.UID
	o.BudgetReserved = true
	o.CreatedAt = now
	o.UpdatedAt = now
	return o, nil
}
func scanOutbox(row interface{ Scan(...any) error }) (OutboxMessage, error) {
	var o OutboxMessage
	var r int
	var ifn, cat, uat int64
	err := row.Scan(&o.ID, &o.BindingID, &o.CustomerID, &o.Generation, &o.UID, &o.Body, &o.ContentRef, &o.State, &o.BudgetUnits, &r, &o.Attempt, &o.ErrorCategory, &o.ExternalMsgID, &ifn, &cat, &uat)
	o.BudgetReserved = r != 0
	o.InFlightAt = timeFrom(ifn)
	o.CreatedAt = timeFrom(cat)
	o.UpdatedAt = timeFrom(uat)
	return o, err
}
func outboxSelect() string {
	return `SELECT id,binding_id,customer_id,generation,uid,body,content_ref,state,budget_units,budget_reserved,attempt,error_category,external_msg_id,in_flight_at,created_at,updated_at FROM outbox`
}
func (s *Store) Outbox(ctx context.Context, id string) (OutboxMessage, error) {
	o, err := scanOutbox(s.db.QueryRowContext(ctx, outboxSelect()+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}
func (s *Store) MarkOutboxSending(ctx context.Context, id string) (OutboxMessage, error) {
	o, err := s.Outbox(ctx, id)
	if err != nil {
		return o, err
	}
	if o.State != OutboxCreated && o.State != OutboxValidated {
		return o, fmt.Errorf("%w: outbox %s -> SENDING", ErrInvalidState, o.State)
	}
	c, err := s.Customer(ctx, o.CustomerID)
	if err != nil {
		return o, err
	}
	if c.Generation != o.Generation || c.UID != o.UID {
		return o, ErrStaleGeneration
	}
	if c.State != CustomerAIEligible {
		return o, ErrHeld
	}
	r, err := s.db.ExecContext(ctx, `UPDATE outbox SET state=?,attempt=attempt+1,in_flight_at=?,updated_at=? WHERE id=?`, OutboxSending, unix(s.now()), unix(s.now()), id)
	if err != nil {
		return o, err
	}
	if err = rowsOrNotFound(r); err != nil {
		return o, err
	}
	return s.Outbox(ctx, id)
}
func (s *Store) TransitionOutbox(ctx context.Context, id, to, errorCategory string) (OutboxMessage, error) {
	if !validOutboxState(to) {
		return OutboxMessage{}, ErrInvalidState
	}
	o, err := s.Outbox(ctx, id)
	if err != nil {
		return o, err
	}
	if !allowedOutbox(o.State, to) {
		return o, fmt.Errorf("%w: outbox %s -> %s", ErrInvalidState, o.State, to)
	}
	r, err := s.db.ExecContext(ctx, `UPDATE outbox SET state=?,error_category=?,updated_at=? WHERE id=?`, to, errorCategory, unix(s.now()), id)
	if err != nil {
		return o, err
	}
	if err = rowsOrNotFound(r); err != nil {
		return o, err
	}
	return s.Outbox(ctx, id)
}
func (s *Store) MarkOutboxUnknown(ctx context.Context, id, category string) (OutboxMessage, error) {
	o, err := s.Outbox(ctx, id)
	if err != nil {
		return o, err
	}
	if o.State != OutboxSending && o.State != OutboxValidated && o.State != OutboxCreated {
		return o, ErrUnknownResult
	}
	r, err := s.db.ExecContext(ctx, `UPDATE outbox SET state=?,error_category=?,updated_at=? WHERE id=?`, OutboxUnknown, category, unix(s.now()), id)
	if err != nil {
		return o, err
	}
	if err = rowsOrNotFound(r); err != nil {
		return o, err
	}
	return s.Outbox(ctx, id)
}
func validOutboxState(v string) bool {
	switch v {
	case OutboxCreated, OutboxValidated, OutboxSending, OutboxUpstreamAccepted, OutboxDeliveryFailed, OutboxUnknown, OutboxRejected, OutboxBlocked:
		return true
	}
	return false
}
func allowedOutbox(a, b string) bool {
	if a == b {
		return true
	}
	switch a {
	case OutboxCreated:
		return b == OutboxValidated || b == OutboxSending || b == OutboxBlocked || b == OutboxRejected
	case OutboxValidated:
		return b == OutboxSending || b == OutboxBlocked || b == OutboxRejected
	case OutboxSending:
		return b == OutboxUpstreamAccepted || b == OutboxDeliveryFailed || b == OutboxUnknown || b == OutboxRejected
	case OutboxUpstreamAccepted:
		return b == OutboxDeliveryFailed
	}
	return false
}
func (s *Store) PendingOutbox(ctx context.Context, bindingID string) ([]OutboxMessage, error) {
	rows, err := s.db.QueryContext(ctx, outboxSelect()+` WHERE binding_id=? AND state IN ('CREATED','VALIDATED','SENDING') ORDER BY created_at,id`, bindingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		o, e := scanOutbox(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) AddAudit(ctx context.Context, key, objType, objID, operation, result, summary string, revision int64) (bool, error) {
	if key == "" {
		return false, ErrInvalidID
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO audit(idempotency_key,object_type,object_id,operation,result,parameter_summary,revision,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, key, objType, objID, operation, result, summary, revision, unix(s.now()))
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func mapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func translateNoRows(err error) error { return err }
func rowsOrNotFound(r sql.Result) error {
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
