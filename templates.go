// templates.go — 管理界面的 HTML 模板(内嵌字符串,零额外文件)
//
// 用 Go html/template,自动转义防 XSS。内联 CSS,无外部依赖。

package main

// adminTemplates 是所有管理页面的 HTML 模板。
var adminTemplates = map[string]string{

	"login": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>登录 - llm-http-proxy</title>
<style>body{font-family:system-ui;margin:40px auto;max-width:400px}
input{display:block;margin:8px 0;padding:8px;width:100%;box-sizing:border-box}
button{padding:10px 20px;cursor:pointer}</style></head>
<body><h2>llm-http-proxy 管理</h2>
<form method="post" action="/__admin/login">
<label>密码:</label>
<input type="password" name="password" autofocus required>
<button type="submit">登录</button>
</form></body></html>`,

	"dashboard": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Dashboard - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>Dashboard</h2>
<table>
<tr><td>版本</td><td>{{.Version}}</td></tr>
<tr><td>编译时间</td><td>{{.BuildTime}}</td></tr>
<tr><td>启动时间</td><td>{{.StartTime}}</td></tr>
<tr><td>运行时长</td><td>{{.Uptime}}</td></tr>
<tr><td>不同 IP 数</td><td>{{.TotalIPs}}</td></tr>
<tr><td>总请求数</td><td>{{.TotalReq}}</td></tr>
<tr><td>Key 配置数</td><td>{{.KeysCount}}</td></tr>
</table>
</body></html>`,

	"keys": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Keys - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>Key 配置 ({{len .}})</h2>
{{if not .}}<p>暂无配置。在下方添加。</p>{{end}}
<table>
<tr><th>Alias</th><th>Header</th><th>Prefix</th><th>Key</th><th>Rate/min</th><th>Burst</th><th>操作</th></tr>
{{range $alias, $cfg := .}}
<tr>
<td><b>{{$alias}}</b></td>
<td>{{$cfg.Header}}</td>
<td>{{$cfg.Prefix}}</td>
<td><code>{{$cfg.Key}}</code></td>
<td>{{if $cfg.Rate}}{{$cfg.Rate}}{{else}}-{{end}}</td>
<td>{{if $cfg.Burst}}{{$cfg.Burst}}{{else}}-{{end}}</td>
<td><form method="post" action="/__admin/keys/delete?alias={{$alias}}" style="display:inline">
<button type="submit" onclick="return confirm('删除 {{$alias}}?')">删除</button></form></td>
</tr>
{{end}}
</table>
<h3>新增 / 编辑</h3>
<form method="post" action="/__admin/keys/new">
<table>
<tr><td>Alias</td><td><input name="alias" placeholder="如 glm" required></td></tr>
<tr><td>Key</td><td><input name="key" style="width:400px" required></td></tr>
<tr><td>Header</td><td><input name="header" placeholder="Authorization(默认)"></td></tr>
<tr><td>Prefix</td><td><input name="prefix" placeholder="Bearer (留空则 Authorization 自动加 Bearer)"></td></tr>
<tr><td>Rate/min</td><td><input name="rate" type="number" placeholder="0=不限流"></td></tr>
<tr><td>Burst</td><td><input name="burst" type="number" placeholder="0=默认"></td></tr>
</table>
<button type="submit">保存</button>
</form>
</body></html>`,

	"stats": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Stats - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>统计 (按 IP)</h2>
{{if not .}}<p>暂无数据。</p>{{else}}
<table>
<tr><th>IP</th><th>不同 Key 数</th><th>总调用</th><th>成功率</th></tr>
{{range $ip, $v := .}}
<tr>
<td><b>{{$ip}}</b></td>
<td>{{$v.DistinctKeys}}</td>
<td>{{$v.TotalCount}}</td>
<td>{{printf "%.1f%%" (mul100 $v.SuccessRate)}}</td>
</tr>
{{end}}
</table>{{end}}
</body></html>`,

	"logs": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Logs - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>最近日志 ({{len .}} 条)</h2>
{{if not .}}<p>暂无日志。</p>{{else}}
<table style="font-size:13px">
<tr><th>时间</th><th>IP</th><th>Key</th><th>Method</th><th>Host</th><th>Status</th><th>耗时</th></tr>
{{range .}}
<tr>
<td>{{.Time}}</td><td>{{.IP}}</td><td>{{.Key}}</td><td>{{.Method}}</td>
<td>{{.Host}}</td><td>{{.Status}}</td><td>{{.Duration}}</td>
</tr>
{{end}}
</table>{{end}}
</body></html>`,

	"msg": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>{{.Title}}</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>{{.Title}}</h2><p>{{.Msg}}</p>
<p><a href="/__admin">返回</a></p>
</body></html>`,
}

// head 是公共 <head> + CSS,所有页面用 {{template "head"}} 引用。
// 注意:head/nav 定义在一个单独的 base 模板里,需要先 Parse 再解析子模板。
// 这里简化:把 head/nav 直接嵌入每个模板太啰嗦,用一个 init 注册 base 模板。
// 但 html/template 的 template 引用需要同一个 template set。
// 简单做法:把 head/nav 也放进 map,渲染时和页面模板一起 Parse。

// baseTemplates 是公共片段(head/nav),渲染时和页面模板合并。
var baseTemplates = `
{{define "head"}}<style>
body{font-family:system-ui;margin:0;padding:0;background:#f5f5f5}
.nav{background:#333;color:#fff;padding:10px 20px;display:flex;gap:15px;align-items:center}
.nav a{color:#8bf;text-decoration:none}
.nav a:hover{text-decoration:underline}
.nav .title{font-weight:bold;margin-right:auto}
.container{padding:20px;max-width:1000px;margin:0 auto}
table{border-collapse:collapse;width:100%;background:#fff;margin:10px 0}
th,td{border:1px solid #ddd;padding:6px 10px;text-align:left}
th{background:#eee}
tr:hover{background:#f0f8ff}
code{background:#eee;padding:2px 4px;border-radius:3px}
input,select{padding:4px}
button{padding:6px 14px;cursor:pointer}
h2{margin-top:0}
</style>{{end}}
{{define "nav"}}<div class="nav">
<span class="title">llm-http-proxy</span>
<a href="/__admin">Dashboard</a>
<a href="/__admin/keys">Keys</a>
<a href="/__admin/stats">Stats</a>
<a href="/__admin/logs">Logs</a>
<a href="/__version" target="_blank">API</a>
<form method="post" action="/__admin/logout" style="display:inline">
<button type="submit" style="padding:4px 10px">登出</button>
</form>
</div><div class="container">{{end}}
`

// mul100 用于模板里把 0-1 的成功率乘 100 显示百分比。
// (Go template 不支持自定义函数直接写在模板里,这里用 Funcs 注册)
