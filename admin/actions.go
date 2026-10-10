package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	agentPattern  = regexp.MustCompile(`^[0-9]{1,10}$`)
	mediaPattern  = regexp.MustCompile(`^[A-Za-z0-9_@.-]{1,256}$`)
	errBadRequest = errors.New("invalid input")
)

// definitive reports whether the official API gave a definite answer
// (business error). Anything else may have executed upstream: UNKNOWN.
func definitive(err error) bool {
	var api *wecom.APIError
	return errors.As(err, &api)
}

func bad(msg string) result { return result{status: http.StatusBadRequest, flash: msg} }

func validName(n string) bool {
	return n != "" && utf8.RuneCountInString(n) <= 16 && !strings.ContainsAny(n, "\r\n\t")
}

// ---- 客服账号 ----

// accountsSync pulls every page first; a failed page writes nothing, so the
// existing list is never cleared by a partial result.
func (c *Console) accountsSync(r *http.Request, _ *session) result {
	ctx := r.Context()
	var all []wecom.Account
	for offset := 0; ; offset += 100 {
		page, err := c.cfg.KF.ListAccounts(ctx, offset, 100)
		if err != nil {
			c.audit(r, "kf_account", "*", "sync", "failed", fmt.Sprintf("page offset %d", offset), 0)
			return result{location: "/admin/accounts", flash: "同步失败，已保留现有列表"}
		}
		all = append(all, page...)
		if len(page) < 100 {
			break
		}
	}
	now := c.cfg.Clock()
	ids := make([]string, 0, len(all))
	for _, a := range all {
		if err := c.cfg.Store.UpsertAccount(ctx, state.KFAccount{OpenKfID: a.OpenKfID, Name: a.Name, URL: a.URL, Status: state.AccountActive, SyncedAt: now}); err != nil {
			return result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
		}
		ids = append(ids, a.OpenKfID)
	}
	// The list is complete (every page succeeded): accounts no longer in it
	// must not stay ACTIVE (#43).
	missing, err := c.cfg.Store.MarkMissingAccounts(ctx, ids)
	if err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	c.audit(r, "kf_account", "*", "sync", "ok", fmt.Sprintf("%d accounts, %d missing", len(all), missing), 0)
	msg := fmt.Sprintf("已同步 %d 个客服账号", len(all))
	if missing > 0 {
		msg += fmt.Sprintf("；%d 个本地账号不在官方列表中，已标记 UNKNOWN（停止新建绑定与发送，请核实）", missing)
	}
	return result{location: "/admin/accounts", flash: msg}
}

func (c *Console) accountCreate(r *http.Request, _ *session) result {
	name, media := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("media_id")
	if !validName(name) || !mediaPattern.MatchString(media) {
		return bad("名称（1-16 字）和头像 media_id 必填")
	}
	id, err := c.cfg.KF.AddAccount(r.Context(), name, media)
	switch {
	case err == nil:
		_ = c.cfg.Store.UpsertAccount(r.Context(), state.KFAccount{OpenKfID: id, Name: name, Status: state.AccountActive, SyncedAt: c.cfg.Clock()})
		c.audit(r, "kf_account", id, "create", "ok", "name="+name, 0)
		return result{location: "/admin/accounts?id=" + id, flash: "已新建客服账号"}
	case definitive(err):
		c.audit(r, "kf_account", "", "create", "rejected", err.Error(), 0)
		return result{location: "/admin/accounts", flash: "微信拒绝新建：" + err.Error()}
	default:
		// The account may exist upstream. Record UNKNOWN; never auto-merge
		// by name or retry the create.
		placeholder := "unknown-" + idemKey(r)
		_ = c.cfg.Store.UpsertAccount(r.Context(), state.KFAccount{OpenKfID: placeholder, Name: name, Status: state.AccountUnknown})
		c.audit(r, "kf_account", placeholder, "create", "unknown", "name="+name, 0)
		return result{location: "/admin/accounts", flash: "新建结果未知（UNKNOWN）：请先同步核实，不要重复新建"}
	}
}

func (c *Console) account(r *http.Request) (state.KFAccount, *result) {
	a, err := c.cfg.Store.Account(r.Context(), r.PathValue("id"))
	if errors.Is(err, state.ErrNotFound) {
		return a, &result{status: http.StatusNotFound, flash: "客服账号不存在"}
	} else if err != nil {
		return a, &result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	if a.Revision != formRev(r) {
		res := conflict("客服账号")
		return a, &res
	}
	return a, nil
}

func (c *Console) markUnknown(r *http.Request, a state.KFAccount, op string) result {
	_, _ = c.cfg.Store.UpdateAccountLocal(r.Context(), a.OpenKfID, a.Revision, func(x *state.KFAccount) { x.Status = state.AccountUnknown })
	c.audit(r, "kf_account", a.OpenKfID, op, "unknown", "", a.Revision)
	return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "微信响应丢失，结果未知（UNKNOWN），相关自动操作已暂停，请同步核实"}
}

func (c *Console) accountEdit(r *http.Request, _ *session) result {
	a, res := c.account(r)
	if res != nil {
		return *res
	}
	name, media, note := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("media_id"), r.PostFormValue("note")
	if !validName(name) || (media != "" && !mediaPattern.MatchString(media)) || utf8.RuneCountInString(note) > 200 {
		return bad("名称或备注无效")
	}
	if name != a.Name || media != "" {
		if err := c.cfg.KF.UpdateAccount(r.Context(), a.OpenKfID, name, media); err != nil {
			if !definitive(err) {
				return c.markUnknown(r, a, "edit")
			}
			c.audit(r, "kf_account", a.OpenKfID, "edit", "rejected", err.Error(), a.Revision)
			return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "微信拒绝修改：" + err.Error()}
		}
	}
	if _, err := c.cfg.Store.UpdateAccountLocal(r.Context(), a.OpenKfID, a.Revision, func(x *state.KFAccount) { x.Name, x.Note = name, note }); err != nil {
		return conflict("客服账号")
	}
	c.audit(r, "kf_account", a.OpenKfID, "edit", "ok", "name="+name, a.Revision)
	return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "已保存"}
}

func (c *Console) accountLink(r *http.Request, _ *session) result {
	a, res := c.account(r)
	if res != nil {
		return *res
	}
	u, err := c.cfg.KF.ContactURL(r.Context(), a.OpenKfID, "admin")
	if err != nil {
		c.audit(r, "kf_account", a.OpenKfID, "link", "failed", "", a.Revision)
		return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "生成链接失败"}
	}
	_, _ = c.cfg.Store.UpdateAccountLocal(r.Context(), a.OpenKfID, a.Revision, func(x *state.KFAccount) { x.URL = u })
	c.audit(r, "kf_account", a.OpenKfID, "link", "ok", "", a.Revision)
	return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "已生成官方客服链接"}
}

// accountDelete needs step-up, a single-use ticket bound to account,
// action and revision, and the typed open_kfid. Deleting does not touch
// bindings or audit.
func (c *Console) accountDelete(r *http.Request, s *session) result {
	a, res := c.account(r)
	if res != nil {
		return *res
	}
	if !c.useTicket(s, r.PostFormValue("ticket"), a.OpenKfID, "delete", a.Revision) || r.PostFormValue("confirm") != a.OpenKfID {
		return result{status: http.StatusForbidden, flash: "危险确认无效或已过期，请重新打开详情页"}
	}
	err := c.cfg.KF.DeleteAccount(r.Context(), a.OpenKfID)
	if err != nil && definitive(err) {
		c.audit(r, "kf_account", a.OpenKfID, "delete", "rejected", err.Error(), a.Revision)
		return result{location: "/admin/accounts?id=" + a.OpenKfID, flash: "微信拒绝删除：" + err.Error()}
	}
	// Deleted or possibly deleted: stop forwarding for this account now.
	n, ferr := c.freezeAccountBindings(r, a.OpenKfID)
	warn := ""
	if ferr != nil {
		warn = "；警告：停用关联绑定失败，请在转发绑定页手动停用"
	}
	if err != nil {
		res := c.markUnknown(r, a, "delete")
		res.flash += fmt.Sprintf("；已停用 %d 个关联绑定", n) + warn
		return res
	}
	_, _ = c.cfg.Store.UpdateAccountLocal(r.Context(), a.OpenKfID, a.Revision, func(x *state.KFAccount) { x.Status = state.AccountDeleted })
	c.audit(r, "kf_account", a.OpenKfID, "delete", "ok", fmt.Sprintf("bindings disabled %d", n), a.Revision)
	return result{location: "/admin/accounts", flash: fmt.Sprintf("已删除微信客服账号；已停用 %d 个关联绑定、吊销其令牌并冻结旧客户 UID（审计保留）", n) + warn}
}

// freezeAccountBindings disables every binding of a deleted account (#32):
// revision bump fences pending sends, ApplyBinding revokes cc-connect
// tokens, and rotating customers makes every old UID stale.
func (c *Console) freezeAccountBindings(r *http.Request, openKfID string) (int, error) {
	all, err := c.cfg.Store.Bindings(r.Context())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range all {
		if b.EnterpriseID != c.cfg.EnterpriseID || b.OpenKfID != openKfID {
			continue
		}
		nb, err := c.cfg.Store.UpdateBinding(r.Context(), b.ID, b.Revision, func(x *state.Binding) { x.Active = false })
		if err != nil {
			return n, err
		}
		if err := c.cfg.Runtime.ApplyBinding(r.Context(), nb, nil); err != nil {
			return n, err
		}
		if _, err := c.cfg.Store.RotateBindingCustomers(r.Context(), b.ID); err != nil {
			return n, err
		}
		c.audit(r, "binding", b.ID, "disable_on_account_delete", "ok", fmt.Sprintf("revision %d -> %d", b.Revision, nb.Revision), b.Revision)
		n++
	}
	return n, nil
}

// ---- 转发绑定 ----

func (c *Console) newCredentials() Credentials {
	return Credentials{CorpID: "bridge_" + c.randomHex(6), Secret: c.randomHex(24), Token: c.randomHex(16), AESKey: strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(c.randomHex(16))), "=")}
}

func (c *Console) saveCreds(ctx context.Context, b state.Binding, cr Credentials) error {
	raw, _ := json.Marshal(cr)
	return c.cfg.Store.SaveBindingSecret(ctx, b.ID, string(raw), b.Revision)
}

func (c *Console) binding(r *http.Request, checkRev bool) (state.Binding, *result) {
	b, err := c.cfg.Store.Binding(r.Context(), r.PathValue("id"))
	if errors.Is(err, state.ErrNotFound) || (err == nil && b.EnterpriseID != c.cfg.EnterpriseID) {
		return b, &result{status: http.StatusNotFound, flash: "绑定不存在"}
	} else if err != nil {
		return b, &result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	if checkRev && b.Revision != formRev(r) {
		res := conflict("绑定")
		return b, &res
	}
	return b, nil
}

func (c *Console) bindingCreate(r *http.Request, _ *session) result {
	id, kf, project, cb, agent := r.PostFormValue("id"), r.PostFormValue("open_kfid"), r.PostFormValue("project_id"), r.PostFormValue("callback_url"), r.PostFormValue("agent_id")
	if !idPattern.MatchString(id) || !idPattern.MatchString(project) || !agentPattern.MatchString(agent) {
		return bad("ID、项目或 agent_id 格式无效")
	}
	if a, err := c.cfg.Store.Account(r.Context(), kf); err != nil || a.Status != state.AccountActive {
		return bad("open_kfid 必须是已同步且状态正常的客服账号")
	}
	if err := c.cfg.Runtime.CheckURL(cb); err != nil {
		return bad("callback URL 不在 allowlist：" + err.Error())
	}
	if _, err := c.cfg.Store.Binding(r.Context(), id); err == nil {
		return result{status: http.StatusConflict, flash: "绑定 ID 已存在"}
	}
	b := state.Binding{ID: id, EnterpriseID: c.cfg.EnterpriseID, OpenKfID: kf, ProjectID: project, CallbackURL: cb, Active: true, Revision: 1}
	if err := c.cfg.Store.PutBinding(r.Context(), b); err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "保存失败"}
	}
	cr := c.newCredentials()
	cr.AgentID = agent
	if err := c.saveCreds(r.Context(), b, cr); err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "保存凭证失败"}
	}
	if err := c.cfg.Runtime.ApplyBinding(r.Context(), b, &cr); err != nil {
		return result{status: http.StatusInternalServerError, flash: "应用绑定失败：" + err.Error()}
	}
	c.audit(r, "binding", id, "create", "ok", "open_kfid="+kf+" project="+project, 1)
	return result{location: "/admin/bindings", flash: "已新建绑定；请用“一次性导出凭证”配置 cc-connect"}
}

func (c *Console) updateBinding(r *http.Request, op string, mut func(*state.Binding)) (state.Binding, *result) {
	b, res := c.binding(r, true)
	if res != nil {
		return b, res
	}
	nb, err := c.cfg.Store.UpdateBinding(r.Context(), b.ID, b.Revision, mut)
	if errors.Is(err, state.ErrConflict) {
		res := conflict("绑定")
		return b, &res
	} else if err != nil {
		return b, &result{status: http.StatusServiceUnavailable, flash: "保存失败"}
	}
	if err := c.cfg.Runtime.ApplyBinding(r.Context(), nb, nil); err != nil {
		return nb, &result{status: http.StatusInternalServerError, flash: "应用绑定失败：" + err.Error()}
	}
	c.audit(r, "binding", b.ID, op, "ok", fmt.Sprintf("revision %d -> %d", b.Revision, nb.Revision), b.Revision)
	return nb, nil
}

func (c *Console) bindingEnable(r *http.Request, _ *session) result {
	if _, res := c.updateBinding(r, "enable", func(b *state.Binding) { b.Active = true }); res != nil {
		return *res
	}
	return result{location: "/admin/bindings", flash: "已启用"}
}

// bindingDisable stops AI forwarding; it never deletes the WeChat account.
func (c *Console) bindingDisable(r *http.Request, _ *session) result {
	if _, res := c.updateBinding(r, "disable", func(b *state.Binding) { b.Active = false }); res != nil {
		return *res
	}
	return result{location: "/admin/bindings", flash: "已停用（旧 revision 的令牌和未发送任务已失效）"}
}

// bindingRebind changes project/callback and gives every customer a new
// generation: old UIDs can never send again.
func (c *Console) bindingRebind(r *http.Request, _ *session) result {
	project, cb := r.PostFormValue("project_id"), r.PostFormValue("callback_url")
	if !idPattern.MatchString(project) {
		return bad("项目格式无效")
	}
	if err := c.cfg.Runtime.CheckURL(cb); err != nil {
		return bad("callback URL 不在 allowlist：" + err.Error())
	}
	b, res := c.updateBinding(r, "rebind", func(b *state.Binding) { b.ProjectID, b.CallbackURL = project, cb })
	if res != nil {
		return *res
	}
	n, err := c.cfg.Store.RotateBindingCustomers(r.Context(), b.ID)
	if err != nil {
		// Binding already moved to the new revision (old one fenced);
		// customers keep their generation until retried.
		return result{status: http.StatusServiceUnavailable, flash: "客户换代失败，请重试改绑"}
	}
	return result{location: "/admin/bindings", flash: fmt.Sprintf("已改绑，%d 个客户已换新代际", n)}
}

// bindingRotate (#40): new secret + revision bump commit together; the
// runtime is switched only after commit; if it refuses, the previous
// credentials are restored so the old version stays usable.
func (c *Console) bindingRotate(r *http.Request, _ *session) result {
	b, res := c.binding(r, true)
	if res != nil {
		return *res
	}
	cr := c.newCredentials()
	oldRaw, oldExported, oldErr := c.cfg.Store.BindingSecret(r.Context(), b.ID)
	if oldErr == nil {
		var old Credentials
		_ = json.Unmarshal([]byte(oldRaw), &old)
		cr.AgentID = old.AgentID
	}
	raw, _ := json.Marshal(cr)
	nb, err := c.cfg.Store.RotateBindingSecret(r.Context(), b.ID, b.Revision, string(raw))
	if errors.Is(err, state.ErrConflict) {
		return conflict("绑定")
	} else if err != nil {
		c.audit(r, "binding", b.ID, "rotate", "failed", "store", b.Revision)
		return result{status: http.StatusServiceUnavailable, flash: "保存凭证失败，旧凭证保持有效"}
	}
	if err := c.cfg.Runtime.ApplyBinding(r.Context(), nb, &cr); err != nil {
		msg := "应用新凭证失败，已恢复旧凭证（旧凭证继续有效）"
		if oldErr != nil {
			msg = "应用新凭证失败；此前无已保存凭证，新凭证将在重启后生效"
		} else if rerr := c.cfg.Store.RestoreBindingSecret(r.Context(), b.ID, oldRaw, oldExported); rerr != nil {
			msg = "应用新凭证失败且恢复旧凭证失败：当前进程仍使用旧凭证，重启后将使用新凭证，请导出新凭证或重新轮换"
		}
		c.audit(r, "binding", b.ID, "rotate", "rolled_back", err.Error(), b.Revision)
		return result{status: http.StatusInternalServerError, flash: msg}
	}
	c.audit(r, "binding", b.ID, "rotate", "ok", fmt.Sprintf("revision %d -> %d", b.Revision, nb.Revision), b.Revision)
	return result{location: "/admin/bindings", flash: "已轮换虚拟凭证，旧凭证与令牌立即失效；请一次性导出新凭证"}
}

func (c *Console) bindingVerify(r *http.Request, _ *session) result {
	b, res := c.binding(r, false)
	if res != nil {
		return *res
	}
	if err := c.cfg.Runtime.Verify(r.Context(), b.ID); err != nil {
		c.audit(r, "binding", b.ID, "verify", "failed", err.Error(), b.Revision)
		return result{location: "/admin/bindings", flash: "回调挑战验证失败：" + err.Error()}
	}
	c.audit(r, "binding", b.ID, "verify", "ok", "", b.Revision)
	return result{location: "/admin/bindings", flash: "回调挑战验证通过"}
}

// bindingExport reveals the generated credentials exactly once, via the
// session (never in the URL), after step-up.
func (c *Console) bindingExport(r *http.Request, s *session) result {
	b, res := c.binding(r, false)
	if res != nil {
		return *res
	}
	raw, err := c.cfg.Store.ExportBindingSecret(r.Context(), b.ID)
	if err != nil {
		c.audit(r, "binding", b.ID, "export", "refused", "", b.Revision)
		return result{status: http.StatusConflict, flash: "凭证已导出过或不存在；如需重新导出请先轮换"}
	}
	var cr Credentials
	_ = json.Unmarshal([]byte(raw), &cr)
	c.mu.Lock()
	s.pendingExport = fmt.Sprintf("[platforms.options]  # cc-connect wecom\ncorp_id = %q\ncorp_secret = %q\ncallback_token = %q\ncallback_aes_key = %q\n", cr.CorpID, cr.Secret, cr.Token, cr.AESKey)
	c.mu.Unlock()
	c.audit(r, "binding", b.ID, "export", "ok", "", b.Revision)
	return result{location: "/admin/bindings"}
}

// ---- 客户与接管 ----

func (c *Console) customer(r *http.Request) (state.Customer, *result) {
	cu, err := c.cfg.Store.Customer(r.Context(), r.PathValue("id"))
	if errors.Is(err, state.ErrNotFound) || (err == nil && cu.EnterpriseID != c.cfg.EnterpriseID) {
		return cu, &result{status: http.StatusNotFound, flash: "客户不存在"}
	} else if err != nil {
		return cu, &result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	if cu.Revision != formRev(r) {
		res := conflict("客户")
		return cu, &res
	}
	return cu, nil
}

// officialTarget returns the service_state mapped to an internal state.
func (c *Console) officialTarget(internal string) (int, bool) {
	best, ok := 0, false
	for k, v := range c.cfg.ServiceStateMap {
		if v == internal && (!ok || k < best) {
			best, ok = k, true
		}
	}
	return best, ok
}

func (c *Console) mapped(n int) string {
	if v, ok := c.cfg.ServiceStateMap[n]; ok {
		return v
	}
	return state.CustomerUnknown
}

// transfer calls service_state/trans, then reads back service_state/get.
// It returns the confirmed internal state, whether the official result is
// certain, and a definitive rejection (nothing changed upstream).
func (c *Console) transfer(r *http.Request, cu state.Customer, internal string, accept ...string) (string, bool, error) {
	target, ok := c.officialTarget(internal)
	if !ok {
		return "", true, errors.New("service_state_map 未配置 " + internal + " 对应的官方状态")
	}
	b, err := c.cfg.Store.Binding(r.Context(), cu.BindingID)
	if err != nil {
		return "", true, errors.New("绑定不存在")
	}
	if err := c.cfg.KF.TransServiceState(r.Context(), b.OpenKfID, cu.ExternalUserID, target); err != nil {
		if definitive(err) {
			return "", true, err
		}
		return "", false, nil
	}
	got, err := c.cfg.KF.ServiceState(r.Context(), b.OpenKfID, cu.ExternalUserID)
	if err != nil {
		return "", false, nil
	}
	m := c.mapped(got)
	for _, a := range accept {
		if m == a {
			return m, true, nil
		}
	}
	return m, false, nil
}

// customerHandover transfers the session officially first and only then
// pauses AI locally. If the official result is uncertain, AI is paused
// (fail-safe) and the customer is marked UNKNOWN.
func (c *Console) customerHandover(r *http.Request, _ *session) result {
	cu, res := c.customer(r)
	if res != nil {
		return *res
	}
	official, certain, rejected := c.transfer(r, cu, state.CustomerWaitingHuman, state.CustomerWaitingHuman, state.CustomerHuman)
	if rejected != nil {
		c.audit(r, "customer", cu.ID, "handover", "rejected", rejected.Error(), cu.Revision)
		return result{location: "/admin/customers?q=" + cu.ID, flash: "转人工失败（官方未变更）：" + rejected.Error()}
	}
	local := state.CustomerHuman
	if !certain {
		official, local = state.CustomerUnknown, state.CustomerUnknown
	}
	if _, err := c.cfg.Store.BeginHandover(r.Context(), cu.ID, "admin_handover"); err != nil {
		return result{status: http.StatusConflict, flash: "转人工失败：" + err.Error()}
	}
	if err := c.cfg.Store.SetHandoverStatus(r.Context(), cu.ID, local, "admin_handover"); err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "转人工状态保存失败"}
	}
	_ = c.cfg.Store.SetOfficialStatus(r.Context(), cu.ID, official)
	if !certain {
		c.audit(r, "customer", cu.ID, "handover", "unknown", "", cu.Revision)
		return result{location: "/admin/customers?q=" + cu.ID, flash: "官方转接结果未知（UNKNOWN）：已在本地暂停 AI，请在企业微信中核实"}
	}
	c.audit(r, "customer", cu.ID, "handover", "ok", "official="+official, cu.Revision)
	return result{location: "/admin/customers?q=" + cu.ID, flash: "已转人工（官方已确认，本地暂停 AI 发送）"}
}

// customerRecover returns the session to the AI assistant officially, reads
// it back, and only then creates the new generation locally.
func (c *Console) customerRecover(r *http.Request, _ *session) result {
	cu, res := c.customer(r)
	if res != nil {
		return *res
	}
	if cu.State != state.CustomerHuman && cu.State != state.CustomerWaitingHuman {
		return result{status: http.StatusConflict, flash: "恢复失败：需先处于已确认的人工状态"}
	}
	official, certain, rejected := c.transfer(r, cu, state.CustomerAIEligible, state.CustomerAIEligible)
	if rejected != nil {
		c.audit(r, "customer", cu.ID, "recover", "rejected", rejected.Error(), cu.Revision)
		return result{location: "/admin/customers?q=" + cu.ID, flash: "恢复失败（官方未变更）：" + rejected.Error()}
	}
	if !certain {
		_ = c.cfg.Store.SetCustomerState(r.Context(), cu.ID, state.CustomerUnknown)
		_ = c.cfg.Store.SetOfficialStatus(r.Context(), cu.ID, state.CustomerUnknown)
		c.audit(r, "customer", cu.ID, "recover", "unknown", "", cu.Revision)
		return result{location: "/admin/customers?q=" + cu.ID, flash: "官方恢复结果未知（UNKNOWN）：AI 保持暂停，请在企业微信中核实"}
	}
	nc, err := c.cfg.Store.RecoverCustomer(r.Context(), cu.ID, "admin_recover")
	if err != nil {
		return result{status: http.StatusConflict, flash: "恢复失败：" + err.Error()}
	}
	_ = c.cfg.Store.SetOfficialStatus(r.Context(), cu.ID, official)
	c.audit(r, "customer", cu.ID, "recover", "ok", fmt.Sprintf("generation %d -> %d", cu.Generation, nc.Generation), cu.Revision)
	return result{location: "/admin/customers?q=" + nc.ID, flash: "已恢复 AI（官方已确认）：新 UID 与新客户端上下文；在途任务不承诺可撤回"}
}

// ---- 诊断 / 设置 ----

func (c *Console) diagMark(r *http.Request, _ *session) result {
	kind, id, note := r.PathValue("kind"), r.PathValue("id"), r.PostFormValue("note")
	if (kind != "inbox" && kind != "outbox") || (note != "已人工核实" && note != "关闭诊断项") || !idPattern.MatchString(id) {
		return bad("诊断标记无效")
	}
	owner, err := c.cfg.Store.DiagnosticEnterprise(r.Context(), kind, id)
	if errors.Is(err, state.ErrNotFound) || (err == nil && owner != c.cfg.EnterpriseID) {
		return result{status: http.StatusNotFound, flash: "诊断项不存在"}
	}
	if err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	if err := c.cfg.Store.MarkDiagnostic(r.Context(), kind, id, note); err != nil {
		return result{status: http.StatusServiceUnavailable, flash: "存储不可用"}
	}
	c.audit(r, kind, id, "diag_mark", "ok", note, 0)
	return result{location: "/admin/diagnostics", flash: "已标记"}
}

func (c *Console) settingsTest(r *http.Request, _ *session) result {
	u := r.PostFormValue("url")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err := c.cfg.Runtime.CheckURL(u)
	if err == nil {
		err = c.cfg.Runtime.TestURL(ctx, u)
	}
	if err != nil {
		c.audit(r, "settings", "callback", "connection_test", "failed", err.Error(), 0)
		return result{location: "/admin/settings", flash: "连接测试失败：" + err.Error()}
	}
	c.audit(r, "settings", "callback", "connection_test", "ok", "", 0)
	return result{location: "/admin/settings", flash: "连接测试通过"}
}
