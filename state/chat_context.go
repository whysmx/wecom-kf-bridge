package state

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// Chat context (handover local draft): open_kfid + external_userid keyed turns
// so reception can see recent AI↔user text when native 智能助手服务记录 cannot.

const (
	ChatRoleUser = "user"
	ChatRoleAI   = "ai"

	DefaultChatContextMaxTurns        = 20
	DefaultChatContextWindowMinutes   = 30
	DefaultChatContextTTLDays         = 7
	DefaultChatContextSummaryMaxChars = 600
)

// ChatTurn is one locally stored user or AI message for handover context.
type ChatTurn struct {
	ID             int64
	OpenKfID       string
	ExternalUserID string
	BindingID      string
	CustomerID     string
	Role           string // ChatRoleUser | ChatRoleAI
	MsgType        string
	Content        string
	CreatedAt      time.Time
}

func normalizeChatWindow(maxTurns, windowMinutes int) (int, int) {
	if maxTurns <= 0 {
		maxTurns = DefaultChatContextMaxTurns
	}
	if windowMinutes <= 0 {
		windowMinutes = DefaultChatContextWindowMinutes
	}
	return maxTurns, windowMinutes
}

func normalizeChatTTL(ttlDays int) int {
	if ttlDays <= 0 {
		return DefaultChatContextTTLDays
	}
	return ttlDays
}

func normalizeSummaryMaxChars(n int) int {
	if n <= 0 {
		return DefaultChatContextSummaryMaxChars
	}
	return n
}

// ChatContentForInbound maps an inbox type+payload to a durable chat draft
// body. Non-text types become short placeholders.
func ChatContentForInbound(msgType, payload string) string {
	switch strings.ToLower(strings.TrimSpace(msgType)) {
	case "text":
		if strings.TrimSpace(payload) != "" {
			return payload
		}
		return "[空文本]"
	case "image":
		return "[图片]"
	case "voice":
		return "[语音]"
	case "file":
		return "[文件]"
	case "video":
		return "[视频]"
	case "":
		return "[消息]"
	default:
		return "[" + msgType + "]"
	}
}

// AppendChatTurn stores one turn for the customer (resolves open_kfid +
// external_userid). Failures are returned to the caller; callers must not
// block handoff on them. Opportunistically purges rows older than TTLDays.
func (s *Store) AppendChatTurn(ctx context.Context, customerID, role, msgType, content string, ttlDays int) error {
	customerID = strings.TrimSpace(customerID)
	role = strings.TrimSpace(role)
	if customerID == "" || (role != ChatRoleUser && role != ChatRoleAI) {
		return ErrInvalidID
	}
	if strings.TrimSpace(msgType) == "" {
		msgType = "text"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		content = ChatContentForInbound(msgType, "")
	}
	var openKf, ext, bindingID string
	err := s.db.QueryRowContext(ctx, `SELECT b.open_kfid, c.external_user_id, c.binding_id FROM customers c JOIN bindings b ON b.id=c.binding_id WHERE c.id=?`, customerID).Scan(&openKf, &ext, &bindingID)
	if err != nil {
		return mapNotFound(err)
	}
	sealed, err := s.sealField(content, "chat_context.content")
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO chat_context(open_kfid,external_userid,binding_id,customer_id,role,msg_type,content,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		openKf, ext, bindingID, customerID, role, msgType, sealed, unix(now))
	if err != nil {
		return err
	}
	// Best-effort TTL purge (global); ignore errors.
	_, _ = s.PurgeChatTurnsOlderThan(ctx, time.Duration(normalizeChatTTL(ttlDays))*24*time.Hour)
	return nil
}

// ListChatTurns returns recent turns for open_kfid+external_userid, oldest first,
// limited by maxTurns and windowMinutes (either bound applies).
func (s *Store) ListChatTurns(ctx context.Context, openKfID, externalUserID string, maxTurns, windowMinutes int) ([]ChatTurn, error) {
	openKfID = strings.TrimSpace(openKfID)
	externalUserID = strings.TrimSpace(externalUserID)
	if openKfID == "" || externalUserID == "" {
		return nil, ErrInvalidID
	}
	maxTurns, windowMinutes = normalizeChatWindow(maxTurns, windowMinutes)
	cutoff := unix(s.now().Add(-time.Duration(windowMinutes) * time.Minute))
	rows, err := s.db.QueryContext(ctx, `SELECT id,open_kfid,external_userid,binding_id,customer_id,role,msg_type,content,created_at FROM chat_context WHERE open_kfid=? AND external_userid=? AND created_at>=? ORDER BY created_at DESC, id DESC LIMIT ?`,
		openKfID, externalUserID, cutoff, maxTurns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatTurn
	for rows.Next() {
		var t ChatTurn
		var cat int64
		if err := rows.Scan(&t.ID, &t.OpenKfID, &t.ExternalUserID, &t.BindingID, &t.CustomerID, &t.Role, &t.MsgType, &t.Content, &cat); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(cat, 0).UTC()
		t.Content, err = s.openField(t.Content, "chat_context.content")
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to oldest-first for display / summary.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListChatTurnsByCustomer resolves the key from the customer row then lists.
func (s *Store) ListChatTurnsByCustomer(ctx context.Context, customerID string, maxTurns, windowMinutes int) ([]ChatTurn, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return nil, ErrInvalidID
	}
	var openKf, ext string
	err := s.db.QueryRowContext(ctx, `SELECT b.open_kfid, c.external_user_id FROM customers c JOIN bindings b ON b.id=c.binding_id WHERE c.id=?`, customerID).Scan(&openKf, &ext)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return s.ListChatTurns(ctx, openKf, ext, maxTurns, windowMinutes)
}

// ClearChatTurns deletes all draft turns for one reception key.
func (s *Store) ClearChatTurns(ctx context.Context, openKfID, externalUserID string) error {
	openKfID = strings.TrimSpace(openKfID)
	externalUserID = strings.TrimSpace(externalUserID)
	if openKfID == "" || externalUserID == "" {
		return ErrInvalidID
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_context WHERE open_kfid=? AND external_userid=?`, openKfID, externalUserID)
	return err
}

// ClearChatTurnsByCustomer clears drafts for the customer's open_kfid+external_userid.
func (s *Store) ClearChatTurnsByCustomer(ctx context.Context, customerID string) error {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return ErrInvalidID
	}
	var openKf, ext string
	err := s.db.QueryRowContext(ctx, `SELECT b.open_kfid, c.external_user_id FROM customers c JOIN bindings b ON b.id=c.binding_id WHERE c.id=?`, customerID).Scan(&openKf, &ext)
	if err != nil {
		return mapNotFound(err)
	}
	return s.ClearChatTurns(ctx, openKf, ext)
}

// PurgeChatTurnsOlderThan deletes turns with created_at before now-ttl.
func (s *Store) PurgeChatTurnsOlderThan(ctx context.Context, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		ttl = time.Duration(DefaultChatContextTTLDays) * 24 * time.Hour
	}
	cutoff := unix(s.now().Add(-ttl))
	r, err := s.db.ExecContext(ctx, `DELETE FROM chat_context WHERE created_at<?`, cutoff)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// FormatChatSummary builds a short multi-line summary of turns (oldest first),
// truncated to maxChars on rune boundaries (prefer whole lines).
func FormatChatSummary(turns []ChatTurn, maxChars int) string {
	maxChars = normalizeSummaryMaxChars(maxChars)
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【近期对话】")
	for _, t := range turns {
		label := "用户"
		if t.Role == ChatRoleAI {
			label = "助手"
		}
		line := "\n" + label + ": " + compactChatLine(t.Content)
		if utf8.RuneCountInString(b.String()+line) > maxChars {
			rest := maxChars - utf8.RuneCountInString(b.String())
			if rest > 8 {
				b.WriteString(truncateRunes(line, rest-1) + "…")
			}
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func compactChatLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if n <= 0 {
			break
		}
		b.WriteRune(r)
		n--
	}
	return b.String()
}

// MergeHandoverTipWithContext appends a local chat summary to the transfer tip.
// On list/format failure returns tip unchanged (never errors out).
func (s *Store) MergeHandoverTipWithContext(ctx context.Context, customerID, tip string, maxTurns, windowMinutes, summaryMaxChars int) string {
	tip = strings.TrimSpace(tip)
	turns, err := s.ListChatTurnsByCustomer(ctx, customerID, maxTurns, windowMinutes)
	if err != nil || len(turns) == 0 {
		return tip
	}
	sum := FormatChatSummary(turns, summaryMaxChars)
	if sum == "" {
		return tip
	}
	if tip == "" {
		return sum
	}
	return tip + "\n" + sum
}
