package admin

import (
	"html/template"
	"net/http"
	"time"
)

var funcs = template.FuncMap{
	"mask": mask,
	"ts": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2006-01-02 15:04:05")
	},
}

// Forms carry csrf_token, idempotency_key (fresh per render) and the
// object's revision. No JavaScript, no third-party assets.
const layout = `{{define "form"}}<input type="hidden" name="csrf_token" value="{{.CSRF}}"><input type="hidden" name="idempotency_key" value="{{.Key}}">{{end}}
{{define "top"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>微信客服网关管理</title>
<style>body{font-family:sans-serif;margin:0;display:flex}nav{width:150px;background:#f3f3f3;min-height:100vh;padding:8px}nav a{display:block;padding:6px}main{padding:12px;flex:1}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:4px 6px;font-size:13px}.flash{background:#ffe;padding:6px;border:1px solid #cc9}.danger{border:2px solid #c33;padding:8px;margin-top:12px}pre{background:#f6f6f6;padding:8px}</style></head><body>
<nav><a href="/admin/">概览</a><a href="/admin/accounts">客服账号</a><a href="/admin/bindings">转发绑定</a><a href="/admin/customers">客户与接管</a><a href="/admin/diagnostics">消息诊断</a><a href="/admin/settings">系统设置</a><a href="/admin/audit">操作审计</a>
<form method="post" action="/admin/logout">{{template "form" .}}<button>退出</button></form></nav><main>
<header><b>{{.Company}}</b> · CorpID {{mask .CorpID}} · 网关 {{index .Status "gateway"}} · 微信客服 API {{index .Status "wecom_api"}} · 最近同步 {{index .Status "last_sync"}}</header>
{{if .Flash}}<p class="flash">{{.Flash}}</p>{{end}}
{{if .Export}}<div class="danger"><b>一次性凭证导出（离开本页后不再显示）</b><pre>{{.Export}}</pre></div>{{end}}
<details><summary>二次认证</summary><form method="post" action="/admin/stepup">{{template "form" .}}<input type="hidden" name="next" value="{{.Path}}"><input type="password" name="password" autocomplete="current-password" required><button>确认密码</button></form></details>{{end}}
{{define "bottom"}}</main></body></html>{{end}}

{{define "login"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>登录</title></head><body><h1>微信客服网关管理</h1>
{{with .}}{{if .Error}}<p>{{.Error}}</p>{{end}}{{end}}
<form method="post" action="/admin/login"><input type="password" name="password" autocomplete="current-password" required><button>登录</button></form></body></html>{{end}}

{{define "overview"}}{{template "top" .}}<h1>概览</h1><table>
{{range $k, $v := .Status}}<tr><th>{{$k}}</th><td>{{$v}}</td></tr>{{end}}
{{range $k, $v := .Data.Counts}}<tr><th>{{$k}}</th><td>{{$v}}</td></tr>{{end}}</table>{{template "bottom"}}{{end}}

{{define "accounts"}}{{template "top" .}}<h1>客服账号</h1>
<form method="get"><input name="q" value="{{.Data.Q}}"><button>搜索</button></form>
<form method="post" action="/admin/accounts/sync">{{template "form" .}}<button>从微信同步</button></form>
<table><tr><th>名称</th><th>open_kfid</th><th>状态</th><th>绑定项目</th><th>官方链接</th><th>最近同步</th><th>revision</th><th></th></tr>
{{range .Data.Accounts}}<tr><td>{{.Name}}</td><td>{{.OpenKfID}}</td><td>{{.Status}}</td><td>{{index $.Data.Projects .OpenKfID}}</td><td>{{.URL}}</td><td>{{ts .SyncedAt}}</td><td>{{.Revision}}</td><td><a href="/admin/accounts?id={{.OpenKfID}}">详情</a></td></tr>{{end}}</table>
<p>共 {{.Data.Total}} 个 · 第 {{.Data.Page}} 页 {{if .Data.Prev}}<a href="?page={{.Data.Prev}}&q={{.Data.Q}}">上一页</a>{{end}} {{if .Data.Next}}<a href="?page={{.Data.Next}}&q={{.Data.Q}}">下一页</a>{{end}}</p>
<h2>新建客服账号</h2><form method="post" action="/admin/accounts/create">{{template "form" .}}名称 <input name="name" maxlength="16" required> 头像 media_id <input name="media_id" required><button>新建</button></form>
{{with .Data.Detail}}<h2>详情：{{.OpenKfID}}</h2>
<form method="post" action="/admin/accounts/{{.OpenKfID}}/edit">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}">名称 <input name="name" value="{{.Name}}" maxlength="16"> 新头像 media_id（可空）<input name="media_id"> 本地备注 <input name="note" value="{{.Note}}" maxlength="200"><button>保存</button></form>
<form method="post" action="/admin/accounts/{{.OpenKfID}}/link">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>生成官方客服链接</button></form>
<div class="danger"><b>危险区域</b>：删除微信客服账号不可恢复，且不同于停用 AI 转发。需先完成二次认证。
<form method="post" action="/admin/accounts/{{.OpenKfID}}/delete">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><input type="hidden" name="ticket" value="{{$.Data.Ticket}}">输入 open_kfid 确认 <input name="confirm" required><button>永久删除</button></form></div>{{end}}
{{template "bottom"}}{{end}}

{{define "bindings"}}{{template "top" .}}<h1>转发绑定</h1>
<table><tr><th>ID</th><th>项目</th><th>open_kfid</th><th>callback URL</th><th>启用</th><th>revision</th><th>操作</th></tr>
{{range .Data.Bindings}}<tr><td>{{.ID}}</td><td>{{.ProjectID}}</td><td>{{.OpenKfID}}</td><td>{{.CallbackURL}}</td><td>{{.Active}}</td><td>{{.Revision}}</td><td>
{{if .Active}}<form method="post" action="/admin/bindings/{{.ID}}/disable">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>停用</button></form>{{else}}<form method="post" action="/admin/bindings/{{.ID}}/enable">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>启用</button></form>{{end}}
<form method="post" action="/admin/bindings/{{.ID}}/verify">{{template "form" $}}<button>回调挑战验证</button></form>
<form method="post" action="/admin/bindings/{{.ID}}/rotate">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>轮换虚拟凭证</button></form>
<form method="post" action="/admin/bindings/{{.ID}}/export">{{template "form" $}}<button>一次性导出凭证</button></form>
<form method="post" action="/admin/bindings/{{.ID}}/rebind">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}">项目 <input name="project_id" value="{{.ProjectID}}"> callback <input name="callback_url" value="{{.CallbackURL}}"><button>改绑（全部客户换代际）</button></form>
</td></tr>{{end}}</table>
<h2>新建绑定</h2><form method="post" action="/admin/bindings/create">{{template "form" .}}ID <input name="id" required> open_kfid <input name="open_kfid" required> 项目 <input name="project_id" required> callback URL <input name="callback_url" required> agent_id <input name="agent_id" value="1000002"><button>新建</button></form>
{{template "bottom"}}{{end}}

{{define "customers"}}{{template "top" .}}<h1>客户与接管</h1>
<form method="get"><input name="q" value="{{.Data.Q}}" placeholder="UID / external_userid / 客户 ID"><button>查询</button></form>
<p>官方接待状态、本地暂停状态和服务存活状态分别显示；恢复 AI 会生成新的 UID 和客户端上下文，已在途的 Codex 任务不承诺可撤回。</p>
<table><tr><th>UID</th><th>昵称</th><th>external_userid</th><th>代际</th><th>官方接待状态</th><th>本地状态</th><th>最近消息</th><th>操作</th></tr>
{{range .Data.Customers}}<tr><td>{{.UID}}</td><td>{{.Nickname}}</td><td>{{mask .ExternalUserID}}</td><td>{{.Generation}}</td><td>{{.OfficialStatus}}</td><td>{{.State}}</td><td>{{ts .LastInboundAt}}</td><td>
<a href="/admin/customers?q={{.ID}}&detail=1">最近消息</a>
<form method="post" action="/admin/customers/{{.ID}}/handover">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>转人工</button></form>
<form method="post" action="/admin/customers/{{.ID}}/recover">{{template "form" $}}<input type="hidden" name="revision" value="{{.Revision}}"><button>恢复 AI（新代际）</button></form></td></tr>{{end}}</table>
{{if .Data.Detail}}<h2>最近 inbox</h2><table>{{range .Data.Inbox}}<tr><td>{{.ID}}</td><td>{{.State}}</td><td>{{.Attempt}}</td><td>{{.ErrorCategory}}</td><td>{{ts .ReceivedAt}}</td></tr>{{end}}</table>
<h2>最近 outbox</h2><table>{{range .Data.Outbox}}<tr><td>{{.ID}}</td><td>{{.State}}</td><td>{{.ChunksSent}}/{{.ChunksTotal}}</td><td>{{.ErrorCategory}}</td><td>{{.BindingRevision}}</td><td>{{ts .CreatedAt}}</td></tr>{{end}}</table>{{end}}
{{template "bottom"}}{{end}}

{{define "diagnostics"}}{{template "top" .}}<h1>消息诊断</h1><p><a href="?view=inbox">Inbox</a> · <a href="?view=outbox">Outbox</a> · <a href="?view=pending">待核查</a>。不显示完整正文；查看正文需二次认证并写审计；不提供重发。</p>
<table><tr><th>类型</th><th>ID</th><th>客户</th><th>状态</th><th>attempt</th><th>分块</th><th>错误类别</th><th>revision</th><th>时间</th><th>核查</th><th></th></tr>
{{range .Data.Items}}<tr><td>{{.Kind}}</td><td>{{.ID}}</td><td>{{.Customer}}</td><td>{{.State}}</td><td>{{.Attempt}}</td><td>{{.Chunks}}</td><td>{{.Error}}</td><td>{{.Revision}}</td><td>{{ts .At}}</td><td>{{.Mark}}</td><td>
<form method="post" action="/admin/diagnostics/{{.Kind}}/{{.ID}}/mark">{{template "form" $}}<select name="note"><option>已人工核实</option><option>关闭诊断项</option></select><button>标记</button></form>
<form method="post" action="/admin/diagnostics/{{.Kind}}/{{.ID}}/view">{{template "form" $}}<button>查看正文（需二次认证）</button></form></td></tr>{{end}}</table>{{template "bottom"}}{{end}}

{{define "body"}}{{template "top" .}}<h1>消息正文：{{.Data.Kind}} {{.Data.ID}}</h1><pre>{{.Data.Body}}</pre>{{template "bottom"}}{{end}}

{{define "settings"}}{{template "top" .}}<h1>系统设置</h1><p>只读展示（保存请修改配置文件并重启，见 README）。Secret/Token/AES 只显示环境变量名。</p>
<table>{{range .Data.Settings}}<tr><th>{{.Group}}</th><td>{{.Name}}</td><td>{{.Value}}</td></tr>{{end}}</table>
<h2>callback 连接测试</h2><form method="post" action="/admin/settings/test">{{template "form" .}}<input name="url" required><button>测试</button></form>{{template "bottom"}}{{end}}

{{define "audit"}}{{template "top" .}}<h1>操作审计</h1><table><tr><th>时间</th><th>操作者</th><th>动作</th><th>对象</th><th>revision</th><th>operation_id</th><th>结果</th><th>摘要</th></tr>
{{range .Data.Entries}}<tr><td>{{ts .CreatedAt}}</td><td>{{.Actor}}</td><td>{{.Operation}}</td><td>{{.ObjectType}}:{{.ObjectID}}</td><td>{{.Revision}}</td><td>{{.Key}}</td><td>{{.Result}}</td><td>{{.Summary}}</td></tr>{{end}}</table>
<p>第 {{.Data.Page}} 页 {{if .Data.Next}}<a href="?page={{.Data.Next}}">下一页</a>{{end}}</p>{{template "bottom"}}{{end}}`

var tmpl = template.Must(template.New("admin").Funcs(funcs).Parse(layout))

type page struct {
	CSRF, Key, Company, CorpID, Flash, Export, Path string
	Status                                          map[string]string
	Data                                            any
}

// render is for the unauthenticated login page only.
func (c *Console) render(w http.ResponseWriter, name string, _ *session, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, name, data)
}

func (c *Console) renderPage(w http.ResponseWriter, r *http.Request, s *session, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	flash, export := c.takeFlash(s)
	p := page{CSRF: s.csrf, Key: c.randomHex(16), Company: c.cfg.CompanyName, CorpID: c.cfg.CorpID, Flash: flash, Export: export, Path: r.URL.Path, Status: c.cfg.Runtime.Status(r.Context()), Data: data}
	if err := tmpl.ExecuteTemplate(w, name, p); err != nil {
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
	}
}
