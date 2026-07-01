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
input[type=checkbox]{display:inline;width:auto;margin-right:6px}
button{padding:10px 20px;cursor:pointer}
.remember{display:flex;align-items:center;margin:4px 0 12px;font-size:14px;color:#555}
.remember input{margin:0 6px 0 0}</style></head>
<body><h2>llm-http-proxy 管理</h2>
<form method="post" action="/__admin/login" autocomplete="on" id="loginForm">
<!-- 隐藏的用户名字段,让浏览器密码管理器能关联保存密码 -->
<input type="text" name="username" value="admin" autocomplete="username" style="display:none">
<label>密码:</label>
<input type="password" name="password" id="pwInput" autocomplete="current-password" autofocus required>
<label class="remember"><input type="checkbox" id="rememberMe">记住密码(仅存本机浏览器)</label>
<button type="submit">登录</button>
</form>
<script>
(function(){
  // 记住密码:勾选后明文存 localStorage(本机浏览器,仅自用,关闭即弃)
  var KEY='llm_proxy_pw';
  var pw=document.getElementById('pwInput');
  var cb=document.getElementById('rememberMe');
  // 页面加载:如果之前存过,自动填充并勾选
  var saved=localStorage.getItem(KEY);
  if(saved){pw.value=saved;cb.checked=true;}
  // 登录提交时根据勾选状态决定存/删
  document.getElementById('loginForm').addEventListener('submit',function(){
    if(cb.checked){localStorage.setItem(KEY,pw.value);}
    else{localStorage.removeItem(KEY);}
  });
})();
</script>
</body></html>`,

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
{{if .QuotaHTML}}
<h2>Key 配额 <a href="/__admin/quota/refresh"><button type="button" style="font-size:13px;padding:3px 12px;vertical-align:middle">⟳ 立即刷新</button></a></h2>
{{.QuotaHTML}}
{{end}}
{{if .UsageHTML}}
<h2>Token 用量统计</h2>
{{.UsageHTML}}
{{end}}
</body></html>`,

	"keys": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Keys - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>Key 配置 ({{len .Aliases}})</h2>
{{if not .Aliases}}<p>暂无配置。在下方添加。</p>{{end}}
<table>
	<tr><th>Alias</th><th>调用地址</th><th>Header</th><th>Prefix</th><th>Key</th><th>Rate/min</th><th>Burst</th><th>Token上限</th><th>窗口</th><th>有效期</th><th>操作</th></tr>
	{{range $alias, $cfg := .Aliases}}
	<tr {{if index $.Expired $alias}}class="expired"{{end}}>
	<td><b>{{$alias}}</b></td>
	<td><code style="font-size:12px" id="url-{{$alias}}">/k/{{$alias}}/</code> <button type="button" onclick="copyURL('{{$alias}}')" style="padding:2px 8px;font-size:12px">复制</button></td>
	<td>{{if $cfg.Header}}{{$cfg.Header}}{{else}}<span style="color:#888">自动</span>{{end}}</td>
	<td>{{$cfg.Prefix}}</td>
	<td><code>{{$cfg.Key}}</code></td>
	<td>{{if $cfg.Rate}}{{$cfg.Rate}}{{else}}-{{end}}</td>
	<td>{{if $cfg.Burst}}{{$cfg.Burst}}{{else}}-{{end}}</td>
	<td>{{if $cfg.MaxTokens}}{{fmtTokens $cfg.MaxTokens}}{{else}}-{{end}}</td>
	<td>{{if $cfg.Window}}{{$cfg.Window}}{{else}}-{{end}}</td>
	<td>{{if $cfg.Expires}}{{$cfg.Expires}}{{if index $.Expired $alias}}<span class="expired-tag">已到期</span>{{end}}{{else}}永久{{end}}</td>
	<td style="white-space:nowrap">
	<a href="/__admin/keys?edit={{$alias}}"><button type="button">编辑</button></a>
	<a href="/__admin/keys?copy={{$alias}}"><button type="button">复制</button></a>
	<form method="post" action="/__admin/keys/delete?alias={{$alias}}" style="display:inline">
	<button type="submit" onclick="return confirm('删除 {{$alias}}?')">删除</button></form>
	</td>
	</tr>
	{{end}}
</table>
<h3>{{if .Editing}}编辑 {{.EditAlias}}{{else if .Copying}}复制自 {{.CopyFrom}}{{else}}新增{{end}}</h3>
<form method="post" action="/__admin/keys/new">
<table>
<tr><td>Alias</td><td>{{if .Editing}}<b>{{.EditAlias}}</b>（不可修改）{{else}}<input name="alias" placeholder="如 glm" required>{{end}}</td></tr>
<tr><td>Key</td><td><input name="key" style="width:400px" value="{{if or .Editing .Copying}}{{.EditCfg.Key}}{{end}}" {{if not .Editing}}required{{end}} placeholder="{{if .Editing}}留空=不修改{{else}}必填{{end}}"></td></tr>
<tr><td>Header</td><td>
<select name="header" onchange="setPrefix(this.value)">
<option value="" {{if or (and (not .Editing) (not .Copying)) (eq .EditCfg.Header "")}}selected{{end}}>自动检测 (推荐)</option>
<option value="Authorization" {{if eq .EditCfg.Header "Authorization"}}selected{{end}}>Authorization (Bearer)</option>
<option value="x-api-key" {{if eq .EditCfg.Header "x-api-key"}}selected{{end}}>x-api-key</option>
<option value="api-key" {{if eq .EditCfg.Header "api-key"}}selected{{end}}>api-key</option>
</select>
<span style="font-size:12px;color:#888;margin-left:8px">自动检测: /anthropic/ 路径用 x-api-key,其他用 Authorization: Bearer</span>
</td></tr>
<tr><td>Prefix</td><td><input name="prefix" id="prefix-input" value="{{if or .Editing .Copying}}{{.EditCfg.Prefix}}{{else}}Bearer {{end}}" placeholder="留空则自动"></td></tr>
<script>
function setPrefix(h) {
  var p = document.getElementById('prefix-input');
  if (h == 'Authorization' && !p.value) p.value = 'Bearer ';
  else if (h != 'Authorization' && p.value == 'Bearer ') p.value = '';
}
// 复制调用地址到剪贴板(用当前页面的 origin + /k/{alias}/ 拼成完整 URL)
function copyURL(alias) {
  var el = document.getElementById('url-' + alias);
  var full = location.origin + el.textContent;
  navigator.clipboard.writeText(full).then(function() {
    var b = event.target; var old = b.textContent;
    b.textContent = '已复制!'; b.disabled = true;
    setTimeout(function(){ b.textContent = old; b.disabled = false; }, 1500);
  });
}
</script>
<tr><td>Rate/min</td><td><input name="rate" type="number" value="{{if or .Editing .Copying}}{{.EditCfg.Rate}}{{end}}" placeholder="0=不限流"></td></tr>
<tr><td>Burst</td><td><input name="burst" type="number" value="{{if or .Editing .Copying}}{{.EditCfg.Burst}}{{end}}" placeholder="0=默认"></td></tr>
<tr><td>有效期</td><td><input name="expires" type="datetime-local" value="{{if or .Editing .Copying}}{{.EditCfg.Expires}}{{end}}" placeholder="留空=永久有效。如 2026-06-22 09:00(北京时间)"></td></tr>
</table>
{{if .Editing}}<input type="hidden" name="alias" value="{{.EditAlias}}">{{end}}
<button type="submit">{{if .Editing}}保存修改{{else if .Copying}}复制为新别名{{else}}保存{{end}}</button>
{{if or .Editing .Copying}}<a href="/__admin/keys"><button type="button">取消</button></a>{{end}}
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
		<div class="table-wrap"><table style="font-size:13px">
			<tr><th>时间</th><th>IP</th><th>Key</th><th>Method</th><th>Host</th><th>Status</th><th>TTFB</th><th>耗时</th><th>流式</th><th>过滤</th><th>乘数</th><th>输入</th><th>缓存</th><th>输出</th><th>命中率</th><th>输入费用</th><th>输出费用</th><th>总费用</th></tr>
			{{range .}}
			<tr {{if ge .Status 400}}style="color:red"{{end}}>
			<td>{{.Time}}</td><td>{{.IP}}</td><td>{{.Key}}</td><td>{{.Method}}</td>
			<td>{{.Host}}</td><td>{{.Status}}</td><td>{{if .TTFB}}{{.TTFB}}{{else}}-{{end}}</td><td>{{.Duration}}</td>
			<td>{{if .Stream}}⚡{{else}}-{{end}}</td>
			<td>{{if .ImageFiltered}}📷{{else}}-{{end}}</td>
		<td>{{if gt .Multiplier 1.0}}×{{printf "%.0f" .Multiplier}}{{else}}-{{end}}</td>
		<td>{{if .Prompt}}{{fmtTokens .Prompt}}{{else}}-{{end}}</td>
		<td>{{if .Cached}}{{fmtTokens .Cached}}{{else}}-{{end}}</td>
		<td>{{if .Completion}}{{fmtTokens .Completion}}{{else}}-{{end}}</td>
	<td>{{if and .Prompt .Cached}}{{printf "%.0f%%" (mul (divf .Cached .Prompt) 100)}}{{else}}-{{end}}</td>
	<td>{{if .CostCalculated}}{{printf "%.6f" .InputCost}}{{else}}-{{end}}</td>
	<td>{{if .CostCalculated}}{{printf "%.6f" .OutputCost}}{{else}}-{{end}}</td>
	<td>{{if .CostCalculated}}{{printf "%.6f" .TotalCost}}{{else}}-{{end}}</td>
	</tr>
	{{end}}
	</table></div>{{end}}
		</body></html>`,

	"daily": `<!DOCTYPE html>
	<html lang="zh-CN"><head><meta charset="utf-8"><title>每日用量 - llm-http-proxy</title>
	{{template "head"}}</head>
	<body>{{template "nav"}}
	<h2>每日用量</h2>
	{{if .}}<p style="color:#555;font-size:14px">按 alias + 日期分组的每日 token 用量与费用。</p>
	{{.}}
	{{else}}<p>暂无每日用量数据。发送请求后会自动记录。</p>{{end}}
	</body></html>`,

	"settings": `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>设置 - llm-http-proxy</title>
{{template "head"}}</head>
<body>{{template "nav"}}
<h2>设置</h2>
<h3>域名白名单</h3>
{{if .WhitelistEnabled}}
<p style="color:#555;font-size:14px">已启用 <b>{{.DomainCount}}</b> 个域名。不在白名单中的域名将被拒绝代理（仅限 key 注入模式）。</p>
<table>
<tr><th>域名</th><th>操作</th></tr>
{{range .Domains}}
<tr>
<td><code>{{.}}</code></td>
<td>
<form method="post" action="/__admin/settings" style="display:inline">
<input type="hidden" name="action" value="remove">
<input type="hidden" name="domain" value="{{.}}">
<button type="submit" onclick="return confirm('确定移出 {{.}}?')">移出</button>
</form>
</td>
</tr>
{{end}}
</table>
{{else}}
<p>白名单当前为空（<b>不限制任何域名</b>）。建议添加常用域名以提高安全性。</p>
{{end}}

<h4>添加域名</h4>
<form method="post" action="/__admin/settings">
<input type="hidden" name="action" value="add">
<input name="domain" placeholder="如 api.z.ai" required style="width:300px">
<button type="submit">添加</button>
</form>

<h3>启动配置（只读）</h3>
<table>
<tr><td>监听地址</td><td><code>{{.Addr}}</code></td></tr>
<tr><td>持久化路径</td><td>{{if .Persist}}{{.Persist}}{{else}}<span style="color:#888">未启用</span>{{end}}</td></tr>
<tr><td>Key 配置</td><td>{{if .Keys}}{{.Keys}}{{else}}<span style="color:#888">未启用（透传模式）</span>{{end}}</td></tr>
<tr><td>管理密码</td><td>{{if .AdminEnabled}}✅ 已启用{{else}}❌ 未启用{{end}}</td></tr>
</table>
</body></html>`,
	"msg": `<!DOCTYPE html>
	<html lang="zh-CN"><head><meta charset="utf-8"><title>{{.Title}}</title>
	{{template "head"}}</head>
	<body>{{template "nav"}}
	<h2>{{.Title}}</h2><p>{{.Msg}}</p>
	<p><a href="/__admin">返回</a></p>
	</body></html>`,

	"config": `<!DOCTYPE html>
	<html lang="zh-CN"><head><meta charset="utf-8"><title>YAML 配置 - llm-http-proxy</title>
	{{template "head"}}</head>
	<body>{{template "nav"}}
	<h2>YAML 配置编辑器</h2>
	{{if .Error}}<p style="color:red;font-weight:bold">⚠ {{.Error}}</p>{{end}}
	<p style="color:#555;font-size:14px">直接编辑 <code>{{.Path}}</code> 文件。<br>
	⚠ 保存时会校验 YAML 语法，校验通过后写入文件并重新加载。建议先备份。</p>
	<form method="post" action="/__admin/config">
	<textarea name="yaml" style="width:100%;height:500px;font-family:monospace;font-size:13px;padding:8px;box-sizing:border-box" spellcheck="false">{{.YAML}}</textarea>
	<br><br>
	<button type="submit">💾 保存配置</button>
	<button type="button" onclick="document.getElementsByName('yaml')[0].scrollTo(0,0)" style="margin-left:8px">回到顶部</button>
	</form>
		</body></html>`,

	"profiles": `<!DOCTYPE html>
	<html lang="zh-CN"><head><meta charset="utf-8"><title>拦截器模板 - llm-http-proxy</title>
	{{template "head"}}</head>
	<body>{{template "nav"}}
	<h2>拦截器模板 ({{if .Profiles}}{{len .Profiles}}{{else}}0{{end}})</h2>
	<p style="color:#555;font-size:14px">拦截器模板(Profiles)定义了可复用的限流/限额/禁止时段参数组合。Alias 通过 <code>profile:</code> 字段引用模板,再用 <code>override:</code> 局部覆盖。</p>
	{{if not .Profiles}}<p>暂无模板。在下方添加。</p>{{end}}
	<table>
	<tr><th>名称</th><th>Rate/min</th><th>Burst</th><th>MaxTokens</th><th>MaxReqs</th><th>Window</th><th>禁止时段</th><th>操作</th></tr>
	{{range $name, $p := .Profiles}}
	<tr>
	<td><b>{{$name}}</b></td>
	<td>{{if $p.Rate}}{{$p.Rate}}{{else}}-{{end}}</td>
	<td>{{if $p.Burst}}{{$p.Burst}}{{else}}-{{end}}</td>
	<td>{{if $p.MaxTokens}}{{$p.MaxTokens}}{{else}}-{{end}}</td>
	<td>{{if $p.MaxReqs}}{{$p.MaxReqs}}{{else}}-{{end}}</td>
	<td>{{if $p.Window}}{{$p.Window}}{{else}}-{{end}}</td>
	<td>{{if $p.TimeBlock}}{{$p.TimeBlock.Start}} ~ {{$p.TimeBlock.End}}{{else}}-{{end}}</td>
	<td style="white-space:nowrap">
	<a href="/__admin/profiles?edit={{$name}}"><button type="button">编辑</button></a>
	<a href="/__admin/profiles?copy={{$name}}"><button type="button">复制</button></a>
	<form method="post" action="/__admin/profiles/delete?name={{$name}}" style="display:inline">
	<button type="submit" onclick="return confirm('删除模板 {{$name}}?')">删除</button></form>
	</td>
	</tr>
	{{end}}
	</table>

	<h3>{{if .Editing}}编辑 {{.EditName}}{{else if .Copying}}复制自 {{.CopyFrom}}{{else}}新增模板{{end}}</h3>
	<form method="post" action="/__admin/profiles/new">
	<table>
	<tr><td>名称</td><td>{{if .Editing}}<b>{{.EditName}}</b>（不可修改）{{else}}<input name="name" placeholder="如 night_block" required>{{end}}</td></tr>
	<tr><td>Rate/min</td><td><input name="rate" type="number" value="{{if or .Editing .Copying}}{{.EditProf.Rate}}{{end}}" placeholder="0=不限流"></td></tr>
	<tr><td>Burst</td><td><input name="burst" type="number" value="{{if or .Editing .Copying}}{{.EditProf.Burst}}{{end}}" placeholder="0=默认"></td></tr>
	<tr><td>MaxTokens</td><td><input name="max_tokens" type="number" value="{{if or .Editing .Copying}}{{.EditProf.MaxTokens}}{{end}}" placeholder="0=不限"></td></tr>
	<tr><td>MaxRequests</td><td><input name="max_requests" type="number" value="{{if or .Editing .Copying}}{{.EditProf.MaxReqs}}{{end}}" placeholder="0=不限"></td></tr>
	<tr><td>Window</td><td><input name="window" placeholder="如 24h, 12h, 7d" value="{{if or .Editing .Copying}}{{.EditProf.Window}}{{end}}"></td></tr>
	<tr><td>禁止时段开始</td><td><input name="time_block_start" placeholder="如 22:00" value="{{if or .Editing .Copying}}{{if .EditProf.TimeBlock}}{{.EditProf.TimeBlock.Start}}{{end}}{{end}}"></td></tr>
	<tr><td>禁止时段结束</td><td><input name="time_block_end" placeholder="如 08:00" value="{{if or .Editing .Copying}}{{if .EditProf.TimeBlock}}{{.EditProf.TimeBlock.End}}{{end}}{{end}}"></td></tr>
	</table>
	{{if .Editing}}<input type="hidden" name="name" value="{{.EditName}}">{{end}}
	<button type="submit">{{if .Editing}}保存修改{{else if .Copying}}复制为新模板{{else}}保存{{end}}</button>
	{{if or .Editing .Copying}}<a href="/__admin/profiles"><button type="button">取消</button></a>{{end}}
	</form>
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
.container{padding:20px;margin:0 auto}
.table-wrap{overflow-x:auto;background:#fff;margin:10px 0;border-radius:4px}
table{border-collapse:collapse;width:100%;background:#fff}
th,td{border:1px solid #ddd;padding:6px 10px;text-align:left;white-space:nowrap}
th{background:#eee;position:sticky;top:0}
tr:hover{background:#f0f8ff}
tr.expired td{opacity:.5}
tr.expired:hover{background:#fff0f0;opacity:.7}
.expired-tag{color:#c00;font-weight:bold;margin-left:6px}
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
	<a href="/__admin/daily">每日</a>
	<a href="/__admin/logs">Logs</a>
<a href="/__admin/settings">设置</a>
<a href="/__admin/profiles">Profiles</a>
<a href="/__admin/config">YAML</a>
<a href="/__version" target="_blank">API</a>
<form method="post" action="/__admin/logout" style="display:inline">
<button type="submit" style="padding:4px 10px">登出</button>
</form>
</div><div class="container">{{end}}
`

// mul100 用于模板里把 0-1 的成功率乘 100 显示百分比。
// (Go template 不支持自定义函数直接写在模板里,这里用 Funcs 注册)
