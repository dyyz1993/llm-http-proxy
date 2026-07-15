// glm-proxy: 百分百透传的通用反向代理,支持 WebSocket。
//
// 用法:把完整目标 URL 拼在代理地址后面即可,其余全部原样。
//
//	http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions?code=xxx
//	      └─ 代理 ┘ └──────── 目标完整 URL,原样拼在后面 ────────┘
//
// 透传内容:method / headers(含 Authorization,不追加任何 header)/ body / query / 流式响应。
//
// 兼容类型:
//   - 普通 HTTP:GET/POST/PUT/DELETE/PATCH...
//   - Body:JSON / 纯文本 / 表单 urlencoded / 文件上传 multipart / 二进制
//   - 流式响应:SSE(chunked 边收边发)
//   - WebSocket:检测 Upgrade 头,升级为双向隧道,两个方向同时拷贝数据
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"llm-http-proxy/internal/cost"
)

// hostFromURL 从目标 URL 字符串里取 host:port。
// 默认端口:http/ws -> 80,https/wss -> 443。
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "wss://") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// tlsDialWithServerName 建立一条 TLS 连接,SNI 用目标 host。
func tlsDialWithServerName(hostport string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return tls.Dial("tcp", hostport, &tls.Config{ServerName: host})
}

// version / buildTime 在构建时通过 -ldflags "-X main.version=... -X main.buildTime=..." 注入。
// startTime 是进程启动时刻(运行时记录)。
var (
	version   = "dev"
	buildTime = "unknown"
	startTime = time.Now() // 进程启动时即记录

	// settingsMgr 管理运行时可修改的服务设置（域名白名单等）。
	settingsMgr *settingsManager

	// usageTracker 按 alias 聚合 token 用量统计(prompt/cached/completion)。
	// 在 proxy handler 里异步解析响应里的 usage 字段后记录,不影响转发延迟。
	usageTracker *usageStats
)

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	persist := flag.String("persist", "", "统计持久化文件路径(为空则不持久化,重启清零)")
	keys := flag.String("keys", "", "key 注入配置文件路径(keys.yaml,为空则不启用 /k/ 模式)")
	adminPw := flag.String("admin-password", "", "管理界面密码(为空则不启用 /__admin)。也可用 ADMIN_PASSWORD 环境变量")
	allowDom := flag.String("allow-domains", "", "key 注入模式的域名白名单(逗号分隔,如 api.z.ai,open.bigmodel.cn)。空=不限制")
	ver := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	// 解析域名白名单:-flag 优先,其次 ALLOW_DOMAINS 环境变量
	var cliDomains []string
	allowSrc := *allowDom
	if allowSrc == "" {
		allowSrc = os.Getenv("ALLOW_DOMAINS")
	}
	if allowSrc != "" {
		for _, d := range strings.Split(allowSrc, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				cliDomains = append(cliDomains, d)
			}
		}
		log.Printf("域名白名单已启用: %d 个域名", len(cliDomains))
	}
	// 初始化运行时设置管理器
	settingsMgr = newSettingsManager()
	if settingsPath := persistSettingsPath(*persist); settingsPath != "" {
		if err := settingsMgr.load(settingsPath); err != nil {
			log.Printf("读取设置持久化失败(将从头开始): %v", err)
		}
	}
	settingsMgr.mergeFromCLI(cliDomains)

	if *ver {
		fmt.Printf("llm-http-proxy %s (built %s)\n", version, buildTime)
		return
	}

	// 管理密码:flag 优先,其次环境变量
	adminPassword := *adminPw
	if adminPassword == "" {
		adminPassword = os.Getenv("ADMIN_PASSWORD")
	}

	stats := newStatsCollector()
	usageTracker = newUsageStats()

	// 持久化:启动时读回历史统计,后台每 30s 落盘一次。
	// stats 和 usage 各用独立文件(避免读写互相影响),路径为 {persist} 和 {persist}.usage。
	if *persist != "" {
		if err := stats.load(*persist); err != nil {
			log.Printf("读取持久化统计失败(将从头开始): %v", err)
		} else {
			log.Printf("已从 %s 读回历史统计", *persist)
		}
		stats.startPersistLoop(*persist, 30*time.Second)

		usagePath := *persist + ".usage"
		if err := usageTracker.load(usagePath); err != nil {
			log.Printf("读取 usage 统计失败(将从头开始): %v", err)
		} else {
			log.Printf("已从 %s 读回历史 usage 统计", usagePath)
		}
		usageTracker.startPersistLoop(usagePath, 30*time.Second)

		// settings 持久化(独立文件,与 stats/usage 共用同一基础路径)
		settingsPath := *persist + ".settings"
		settingsMgr.startPersistLoop(settingsPath, 30*time.Second)
	}

	// key 注入模式:加载 keys.yaml + 启动热加载
	var ks *keyStore
	if *keys != "" {
		ks = newKeyStore()
		if err := ks.load(*keys); err != nil {
			log.Printf("加载 key 配置失败: %v", err)
		} else {
			log.Printf("已从 %s 加载 key 配置", *keys)
		}
		ks.startReloadLoop(10 * time.Second)
	}

	// 配额缓存(只在 key 注入模式下启用,后台定时轮询 api.z.ai)
	var quotaCacheInst *quotaCache
	if ks != nil {
		quotaCacheInst = newQuotaCache(*addr)
		quotaCacheInst.startLoop(ks)
	}

	// 管理界面(密码非空才启用)
	var admin *adminServer
	if adminPassword != "" {
		admin = newAdminServer(adminPassword, stats, ks, quotaCacheInst, usageTracker, settingsMgr, *addr, *persist, *keys)
		log.Printf("管理界面已启用: http://localhost%s/__admin", *addr)
	}

	// 顶层路由。
	topHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// /__admin/* 路由(管理界面,优先匹配)
		if admin != nil && (req.URL.Path == "/__admin" || strings.HasPrefix(req.URL.Path, "/__admin/")) {
			admin.handler().ServeHTTP(w, req)
			return
		}
		switch {
		case req.URL.Path == "/__version":
			// 启用了管理界面时,/__version 也要求登录(防止泄露版本/uptime 等运行时信息)
			if admin != nil && !admin.authCheck(req) {
				http.Redirect(w, req, "/__admin/login", http.StatusSeeOther)
				return
			}
			versionHandler(w, req)
			return
		case req.URL.Path == "/__stats":
			// 统计端点暴露了 key 别名、用量等敏感信息,必须登录才能查看。
			// 未配置管理员密码时直接拒绝,防止信息泄露。
			if admin == nil {
				http.Error(w, `{"error":"stats require admin authentication (set ADMIN_PASSWORD)"}`, http.StatusUnauthorized)
				return
			}
			statsHandler(stats, admin.authCheck).ServeHTTP(w, req)
			return
		case req.URL.Path == "/__quota":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if req.Method == "POST" {
				// POST: 从 body 读取 raw keys 直接查询
				handleQuotaPost(w, req)
				return
			}
			// GET: 从 keys.yaml 缓存查询
			if quotaCacheInst == nil {
				w.Write([]byte("配额缓存未启用（需要 key 注入模式）\n"))
				return
			}
			// 可选 refresh=1 强制刷新
			if req.URL.Query().Get("refresh") == "1" {
				quotaCacheInst.fetchAll(ks.allConfigs())
			}
			entries := quotaCacheInst.getAll()
			// 可选 alias=xxx 过滤
			filters := req.URL.Query()["alias"]
			w.Write([]byte(buildSortedQuotaText(entries, filters...)))
			return
		case req.URL.Path == "/" || req.URL.Path == "":
			// 根路径返回使用指南(TXT)
			serveHelp(w, "")
			return
		case ks != nil && strings.HasPrefix(req.URL.Path, "/g/"):
			// 群组模式: /g/{group}/https://目标
			if strings.HasSuffix(req.URL.Path, "/__stats") {
				handleAliasStats(w, req, ks, stats, usageTracker)
				return
			}
			handleGroupRoutePrefix(w, req, ks, stats, usageTracker)
			return
		case ks != nil && strings.HasPrefix(req.URL.Path, "/k/"):
			if strings.HasSuffix(req.URL.Path, "/__stats") {
				// 按别名统计: /k/{alias}/__stats 无需额外认证,知道 alias 即可查看
				handleAliasStats(w, req, ks, stats, usageTracker)
				return
			}
			// key 注入模式:/k/{alias}/https://目标
			handleKeyRoute(w, req, ks, stats, usageTracker)
			return
		default:
			// 纯透传模式:/https://目标
			ctx := &CheckContext{
				Store: ks,
			}
			runChecks(w, passthroughChecks, ctx)
			newProxyHandler(stats, nil, "", ctx.ImageFilter, ctx.TokenMultipliers, ctx.RetryConfig).ServeHTTP(w, req)
		}
	})

	log.Printf("透传代理已启动: http://localhost%s  (支持 HTTP / SSE / WebSocket)", *addr)
	log.Printf("版本: %s (built %s)", version, buildTime)
	log.Printf("统计查看: http://localhost%s/__stats", *addr)
	if err := http.ListenAndServe(*addr, topHandler); err != nil {
		log.Fatal(err)
	}
}

// versionInfo 是 /__version 返回的结构。
type versionInfo struct {
	Version   string `json:"version"`    // 版本号(tag 注入,如 v1.5.0)
	BuildTime string `json:"build_time"` // 编译时刻
	StartTime string `json:"start_time"` // 进程启动时刻
	Uptime    string `json:"uptime"`     // 已运行时长(人类可读)
}

// versionHandler 处理 GET /__version,返回版本号、编译时间、启动时间、运行时长。
func versionHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uptime := time.Since(startTime)
	info := versionInfo{
		Version:   version,
		BuildTime: buildTime,
		StartTime: startTime.In(beijing).Format(time.RFC3339),
		Uptime:    uptime.Round(time.Second).String(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(info)
}

// helpText 返回纯文本使用教程(分章节)。根路径 / 裸 alias 路径访问时展示。
// 接收 alias 参数:为空时是通用教程,非空时在示例里用该 alias。
func helpText(alias string) string {
	aliasExample := "your-alias" // 占位符,不暴露真实别名
	if alias != "" {
		aliasExample = alias
	}
	return fmt.Sprintf(`# llm-http-proxy 使用指南

100%% 透传的反向代理,支持 GLM / OpenAI / Claude 等 LLM API。
HTTP / SSE / WebSocket 全透传,不追加任何 header,不修改响应体。
两个用法:【别名模式】服务端注入 key(推荐),【透传模式】客户端自带 key。

---

## 一、快速开始(别名模式 / 推荐)

别名模式: /k/{别名}/目标完整URL
真实 API key 只存在服务端,客户端用别名调用,key 不外泄。

【推荐】走 api.z.ai(国际域名,响应更稳,权限/计费更友好):

  # OpenAI 兼容格式
  curl https://p.19930810.xyz:8443/k/%[1]s/https://api.z.ai/api/coding/paas/v4/chat/completions \
    -H "Authorization: Bearer 任意值" \
    -H "Content-Type: application/json" \
    -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"你好"}]}'

  # Anthropic 兼容格式(Claude SDK / 库可直接用,glm-4.6 也能调)
  curl https://p.19930810.xyz:8443/k/%[1]s/https://api.z.ai/api/anthropic/v1/messages \
    -H "x-api-key: 任意值" \
    -H "anthropic-version: 2023-06-01" \
    -H "Content-Type: application/json" \
    -d '{"model":"glm-4.6","messages":[{"role":"user","content":"你好"}],"max_tokens":50}'

也可走官方域名 open.bigmodel.cn(同后端,路径相同):

  curl https://p.19930810.xyz:8443/k/%[1]s/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
    -H "Authorization: Bearer 任意值" \
    -H "Content-Type: application/json" \
    -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"你好"}]}'

---

## 二、透传模式(客户端自带 key)

把完整目标 URL 直接拼在路径里,key 由客户端提供:

  curl https://p.19930810.xyz:8443/https://api.z.ai/api/coding/paas/v4/chat/completions \
    -H "Authorization: Bearer 你的真实KEY" \
    -H "Content-Type: application/json" \
    -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"你好"}]}'

---

## 三、原理说明

1. 本服务是纯透传反向代理。收到请求后:
   - 别名模式:从服务端配置查到别名对应的真实 key,覆盖进请求头,再转发;
   - 透传模式:原样转发,不改 header。
2. Header 自动检测:客户端带 x-api-key 就替换 x-api-key,
   带 Authorization 就替换 Authorization,两个都带就都替换。
3. 支持的三种上游路径(同一套 GLM 后端,优先用 api.z.ai):
   - api.z.ai/api/coding/paas/v4/...        (OpenAI 格式,推荐 / 编程套餐)
   - api.z.ai/api/anthropic/v1/...          (Anthropic 格式,推荐)
   - open.bigmodel.cn/api/coding/paas/v4/... (OpenAI 格式,官方域名 / 编程套餐)
   - open.bigmodel.cn/api/anthropic/v1/... (Anthropic 格式,官方域名)
4. SSE 流式 / WebSocket 全程透传,边收边转发,不缓冲不修改。
5. key 不会出现在日志、统计、URL 里;统计只用别名/掩码 key。

---

## 四、状态码说明

  200          正常
  499          客户端主动断连(自己超时取消了请求,非上游问题)
  502          上游转发失败(真正的网络/上游错误)
  429          请求过于频繁(别名限流)
  410          别名已过期

---

## 五、源码 / 反馈

GitHub: https://github.com/dyyz1993/llm-http-proxy
`, aliasExample)
}

// serveHelp 返回 TXT 教程(text/plain),状态码 200。
// alias 参数用于在示例里替换别名,为空则用默认 "glm"。
func serveHelp(w http.ResponseWriter, alias string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, helpText(alias))
}

// handleKeyRoute 处理 key 注入模式: /k/{alias}/https://目标
// 从 keyStore 查 alias 配置,经拦截器链(禁止时段/用量限额/限流/白名单)审查,
// 通过后注入真实 key 到 header,然后走转发。
// 用户不需要带 key(带了也会被配置的 key 覆盖)。
func handleKeyRoute(w http.ResponseWriter, req *http.Request, ks *keyStore, stats *statsCollector, us *usageStats) {
	// 原始 RequestURI 形如: /k/glm/https://open.bigmodel.cn/api/...
	// 去掉前导 / 得: k/glm/https://open.bigmodel.cn/api/...
	raw := strings.TrimPrefix(req.RequestURI, "/")

	// 解析 k/{alias}/{target}
	// 先去掉 "k/" 前缀
	rest := strings.TrimPrefix(raw, "k/")
	if rest == raw || rest == "" {
		// 裸 /k/ → 返回通用教程
		serveHelp(w, "")
		return
	}
	// 取第一个 / 之前的部分作为 alias
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// 裸 /k/{alias}(无目标)→ 返回带该 alias 的教程
		serveHelp(w, rest)
		return
	}
	alias := rest[:slash]
	target := rest[slash+1:]
	if target == "" || !strings.Contains(target, "://") {
		// 裸 /k/{alias}/ (无目标) 或目标格式不对 → 返回带该 alias 的教程
		serveHelp(w, alias)
		return
	}

	// 查 alias 配置(lookup 内含过期检查)
	cfg, ok := ks.lookup(alias)
	if !ok {
		// 检查是否是 group
		if ks.isGroup(alias) {
			handleGroupRoute(w, req, ks, stats, us, alias, target)
			return
		}
		// 区分"不存在"和"已过期"
		if ks.isExpired(alias) {
			http.Error(w, "此 key 标识已过期: "+alias+"\n", http.StatusGone)
		} else {
			http.Error(w, "未知的 key 标识: "+alias+"\n", http.StatusNotFound)
		}
		return
	}

	// 合并拦截器模板(profile → override → alias 直接字段)
	cfg = resolveConfig(cfg, ks.getInterceptorProfiles(), "default")

	// 构建拦截器上下文
	ctx := &CheckContext{
		Alias:    alias,
		Target:   target,
		Domain:   extractDomain(target),
		Config:   cfg,
		Request:  req,
		Store:    ks,
		Usage:    us,
		Settings: settingsMgr,
	}

	// 走拦截器链
	if !runChecks(w, keyRouteChecks, ctx) {
		return
	}

	// 全部通过,转发
	req.RequestURI = "/" + target
	newProxyHandler(stats, ctx.HeadersToInject, ctx.StatLabel,
		ctx.ImageFilter, ctx.TokenMultipliers, ctx.RetryConfig).ServeHTTP(w, req)
}

// handleGroupRoutePrefix 解析 /g/{group}/https://target 并交给 handleGroupRoute。
func handleGroupRoutePrefix(w http.ResponseWriter, req *http.Request, ks *keyStore, stats *statsCollector, us *usageStats) {
	raw := strings.TrimPrefix(req.RequestURI, "/")
	rest := strings.TrimPrefix(raw, "g/")
	if rest == raw || rest == "" {
		serveHelp(w, "")
		return
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		serveHelp(w, rest)
		return
	}
	groupName := rest[:slash]
	target := rest[slash+1:]
	if target == "" || !strings.Contains(target, "://") {
		serveHelp(w, groupName)
		return
	}
	handleGroupRoute(w, req, ks, stats, us, groupName, target)
}

// handleGroupRoute 处理 group 别名池请求。
// 按成员优先级依次尝试,失败(拦截器拒绝或上游返回 on_status 码)则换下一个成员。
func handleGroupRoute(w http.ResponseWriter, req *http.Request, ks *keyStore, stats *statsCollector, us *usageStats, groupName, target string) {
	gm := ks.getGroupManager()
	if gm == nil {
		http.Error(w, "group 功能未启用\n", http.StatusInternalServerError)
		return
	}

	groups := ks.getGroups()
	cfg, ok := groups[groupName]
	if !ok {
		http.Error(w, "未知的 group: "+groupName+"\n", http.StatusNotFound)
		return
	}

	log.Printf("group 请求: group=%s members=%v target=%s", groupName, cfg.Members, target[:min(60, len(target))])

	// 预读请求 body(POST 可重放):第一次尝试后 body 被消耗,后续成员需要重建。
	var bodyBuf []byte
	if req.Body != nil {
		bodyBuf, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}

	for _, member := range cfg.Members {
		// 每次迭代重建 req.Body(上一次尝试可能已消耗)
		if bodyBuf != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			req.ContentLength = int64(len(bodyBuf))
		}

		// 跳过冷却中的成员
		st := gm.memberStatus(member)
		if st.IsCooling {
			log.Printf("group 跳过冷却成员: group=%s member=%s 冷却剩余=%v",
				groupName, member, time.Until(st.CoolUntil).Round(time.Second))
			continue
		}

		// 查成员配置
		memberCfg, ok := ks.lookup(member)
		if !ok {
			log.Printf("group 成员不存在或已过期: group=%s member=%s", groupName, member)
			continue
		}
		memberCfg = resolveConfig(memberCfg, ks.getInterceptorProfiles(), "default")

		// 拦截器链检查(禁止时段/限额/限流/白名单)
		ctx := &CheckContext{
			Alias:    member,
			Target:   target,
			Domain:   extractDomain(target),
			Config:   memberCfg,
			Request:  req,
			Store:    ks,
			Usage:    us,
			Settings: settingsMgr,
		}

		rec := newBufferedWriter()
		if !runChecks(rec, keyRouteChecks, ctx) {
			// 拦截器拒绝了(402/403/429等) → 标记冷却,换下一个
			gm.markCooldown(member, groupName, rec.status)
			log.Printf("group 成员被拦截器拒绝: group=%s member=%s status=%d",
				groupName, member, rec.status)
			continue
		}

		// 拦截器通过 → 用 buffering writer 转发请求
		// buffering writer 先把响应存到内存,判断状态码后再决定 flush 给客户端还是丢弃换人
		req.RequestURI = "/" + target
		newProxyHandler(stats, ctx.HeadersToInject, ctx.StatLabel,
			ctx.ImageFilter, ctx.TokenMultipliers, ctx.RetryConfig).ServeHTTP(rec, req)

		// 检查响应状态码
		if rec.status >= 200 && rec.status < 400 {
			// 成功 → flush buffer 给客户端,返回
			rec.flushTo(w)
			gm.markSuccess(member, rec.status)
			return
		}

		if gm.shouldSwitchStatus(groupName, rec.status) {
			// 可换人状态码 → 丢弃 buffer,标记冷却,换下一个
			gm.markCooldown(member, groupName, rec.status)
			log.Printf("group 成员返回 %d → 换人: group=%s member=%s", rec.status, groupName, member)
			continue
		}

		if rec.status == 0 {
			// 没拿到响应(连接错误等) → 换人
			gm.markCooldown(member, groupName, 502)
			log.Printf("group 成员无响应: group=%s member=%s → 换人", groupName, member)
			continue
		}

		// 其他状态码(不在 on_status 里)→ flush 给客户端,正常返回
		rec.flushTo(w)
		return
	}

	// 全部成员都失败了
	log.Printf("group 全部成员不可用: group=%s", groupName)
	w.Header().Set("Retry-After", "60")
	http.Error(w, "所有上游成员暂时不可用,请稍后重试 (group: "+groupName+")\n", http.StatusServiceUnavailable)
}

// bufferedWriter 先把响应存到内存,判断状态码后再决定是否 flush 给客户端。
// 用于 group 路由:成员返回可换人状态码时丢弃 buffer,换下一个成员重试。
type bufferedWriter struct {
	header  http.Header
	status  int
	body    []byte
	written bool // 是否已 flush(防止重复)
}

func newBufferedWriter() *bufferedWriter {
	return &bufferedWriter{header: make(http.Header)}
}

func (b *bufferedWriter) Header() http.Header {
	return b.header
}

func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = 200 // 隐式 200
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

// flushTo 把缓冲的响应写入真正的 ResponseWriter。
func (b *bufferedWriter) flushTo(w http.ResponseWriter) {
	if b.written {
		return
	}
	b.written = true
	dst := w.Header()
	for k, vs := range b.header {
		dst[k] = vs
	}
	w.WriteHeader(b.status)
	w.Write(b.body)
}

// Flush 实现 http.Flusher 接口(buffering 模式下是 no-op,最终统一 flushTo)。
func (b *bufferedWriter) Flush() {}

// handleAliasStats 处理 /k/{alias}/__stats,返回该别名的用量统计和最近日志。
// 不要求额外认证——知道 alias 就能看(和能用这个 alias 的范围一致)。
func handleAliasStats(w http.ResponseWriter, r *http.Request, ks *keyStore, stats *statsCollector, us *usageStats) {
	// 路径: /k/{alias}/__stats
	path := strings.TrimPrefix(r.URL.Path, "/k/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] != "__stats" {
		http.Error(w, "invalid stats path", http.StatusBadRequest)
		return
	}
	alias := parts[0]

	allCfg := ks.allConfigs()
	if _, ok := allCfg[alias]; !ok {
		http.Error(w, "unknown alias: "+alias, http.StatusNotFound)
		return
	}
	expired := ks.isExpired(alias)
	statKey := "key:" + alias

	// Token 用量统计
	usageSnap := us.snapshot()
	var usageHTML string
	if u, ok := usageSnap[alias]; ok {
		usageHTML = buildUsageHTML(map[string]aliasUsageStats{alias: u}, nil)
	}

	// 请求统计(从 statsCollector 过滤出该别名的条目)
	statsSnap := stats.snapshot()
	var totalReqs int64
	var ok2xx int64
	ipSet := make(map[string]bool)
	for ip, is := range statsSnap {
		if ke, ok := is.Keys[statKey]; ok {
			totalReqs += ke.Count
			for code, c := range ke.StatusCounts {
				if code >= 200 && code < 300 {
					ok2xx += c
				}
			}
			ipSet[ip] = true
		}
	}
	var successRate float64
	if totalReqs > 0 {
		successRate = float64(ok2xx) / float64(totalReqs) * 100
	}

	// 最近日志(过滤出该别名的条目)
	logs := globalLogRing.recent(200)
	var aliasLogs []logEntry
	for _, e := range logs {
		if e.Key == statKey {
			aliasLogs = append(aliasLogs, e)
		}
	}
	if len(aliasLogs) > 50 {
		aliasLogs = aliasLogs[:50]
	}

	// 渲染 HTML
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>%s - 别名统计 - llm-http-proxy</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;margin:20px;background:#f5f5f5;color:#333}
h2{border-bottom:2px solid #4a90d9;padding-bottom:6px}
table{border-collapse:collapse;width:auto;margin:8px 0;background:#fff;font-size:13px}
th,td{border:1px solid #ddd;padding:6px 10px;text-align:center}
th{background:#4a90d9;color:#fff;font-weight:600;white-space:nowrap}
tr:nth-child(even){background:#f9f9f9}
.summary{display:flex;gap:16px;margin:12px 0;flex-wrap:wrap}
.card{background:#fff;border:1px solid #ddd;border-radius:6px;padding:14px 20px;min-width:140px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.08)}
.card .num{font-size:24px;font-weight:700;color:#4a90d9}
.card .label{font-size:12px;color:#888;margin-top:4px}
.expired-badge{display:inline-block;background:#e74c3c;color:#fff;padding:2px 10px;border-radius:4px;font-size:12px;font-weight:700}
.status-ok{color:#27ae60;font-weight:700}
.status-err{color:#e74c3c}
.log-table{font-size:12px}
.log-table td{font-family:monospace;font-size:11px}
a{color:#4a90d9;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head><body>
<h2>别名统计: %s %s</h2>
`, alias, alias, alias)
	if expired {
		fmt.Fprintf(w, `<p><span class="expired-badge">已过期</span></p>`)
	}

	// 统计卡片
	fmt.Fprintf(w, `<div class="summary">
<div class="card"><div class="num">%d</div><div class="label">总请求</div></div>
<div class="card"><div class="num">%d</div><div class="label">来源 IP</div></div>
<div class="card"><div class="num">%s</div><div class="label">成功率</div></div>
</div>`, totalReqs, len(ipSet), fmt.Sprintf("%.1f%%", successRate))

	// Token 用量表
	if usageHTML != "" {
		fmt.Fprintf(w, `<h3>Token 用量</h3>%s`, usageHTML)
	} else {
		fmt.Fprintf(w, `<p>暂无 Token 用量数据。</p>`)
	}

	// 日志表
	fmt.Fprintf(w, `<h3>最近 %d 条请求日志</h3>`, len(aliasLogs))
	if len(aliasLogs) == 0 {
		fmt.Fprintf(w, `<p>暂无日志。</p>`)
	} else {
		fmt.Fprintf(w, `<table class="log-table">
<tr><th>时间</th><th>IP</th><th>Method</th><th>Host</th><th>Status</th><th>TTFB</th><th>耗时</th><th>流式</th><th>输入</th><th>缓存</th><th>输出</th><th>费用</th></tr>`)
		for _, e := range aliasLogs {
			statusClass := ""
			if e.Status >= 400 {
				statusClass = ` class="status-err"`
			} else if e.Status >= 200 && e.Status < 300 {
				statusClass = ` class="status-ok"`
			}
			streamMark := ""
			if e.Stream {
				streamMark = "⚡"
			}
			costStr := "-"
			if e.CostCalculated && e.TotalCost > 0 {
				costStr = fmt.Sprintf("%.4f", e.TotalCost)
			}
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td%s>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				e.Time, e.IP, e.Method, e.Host, statusClass, e.Status,
				e.TTFB, e.Duration, streamMark,
				fmtTokens(e.Prompt), fmtTokens(e.Cached), fmtTokens(e.Completion),
				costStr)
		}
		fmt.Fprintf(w, `</table>`)
	}

	fmt.Fprintf(w, `<p style="color:#888;font-size:12px;margin-top:20px">
	<a href="/k/%[1]s/__stats">↻ 刷新</a> · 
	<a href="/">使用指南</a></p>
	</body></html>`, alias)
}

// handleQuotaPost 处理 POST /__quota,从请求体读取 raw keys 直接查询配额。
// 请求体: {"keys":["key1","key2",...]} 或 ["key1","key2",...]
func handleQuotaPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB
	if err != nil {
		w.Write([]byte("读取请求体失败\n"))
		return
	}

	// 尝试解析为 {"keys": [...]}
	var payload struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Keys) == 0 {
		// 尝试解析为直接数组 ["key1", "key2", ...]
		var arrKeys []string
		if err := json.Unmarshal(body, &arrKeys); err != nil || len(arrKeys) == 0 {
			w.Write([]byte("请求体格式错误: 需要 {\"keys\":[\"...\"]} 或 [\"...\"]\n"))
			return
		}
		payload.Keys = arrKeys
	}

	if len(payload.Keys) > 50 {
		w.Write([]byte("一次最多查询 50 个 key\n"))
		return
	}

	// 逐个查询
	var entries []cachedQuota
	for i, rawKey := range payload.Keys {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" || strings.HasPrefix(rawKey, "sk-") || len(rawKey) < 20 {
			continue
		}
		label := fmt.Sprintf("key-%d", i+1)
		if e := fetchOneKey(label, rawKey); e != nil {
			entries = append(entries, *e)
		}
	}

	if len(entries) == 0 {
		w.Write([]byte("所有 key 查询失败(检查 key 是否有效)\n"))
		return
	}

	w.Write([]byte(buildSortedQuotaText(entries)))
}

// sharedTransport / sharedClient 是全局共享的连接池。
// 之前每个请求都 new 一个 Transport+Client,导致连接池失效、每次都 TCP/TLS 握手。
// 改为全局复用后,空闲连接可 keep-alive,大幅减少延迟和资源消耗。
var sharedTransport = &http.Transport{
	DisableCompression:  true, // 不偷偷加 Accept-Encoding: gzip
	MaxIdleConnsPerHost: 20,   // 每个 host 最多保持 20 条空闲连接
	IdleConnTimeout:     90 * time.Second,
}
var sharedClient = &http.Client{Transport: sharedTransport}

// newProxyHandler 构造代理的 http.Handler。测试和 main 共用同一份逻辑。
// stats 非 nil 时,记录每次请求的 IP/掩码key/状态码统计。
// injectHeaders 非 nil 时(key 注入模式),这些 header 会在转发前覆盖进请求头。
// statKeyLabel 用于统计里替代掩码 key 显示(如 key 注入模式显示 alias "glm")。
// imageFilter 是 image_url 过滤规则,非空时在转发前过滤请求 body。
// tokenMultipliers 是 Token 用量乘数规则,在提取 usage 后应用。
// retryCfg 是上游重试配置(零值=不重试)。
func newProxyHandler(stats *statsCollector, injectHeaders http.Header, statKeyLabel string, imageFilter []ImageFilterRule, tokenMultipliers []TokenMultiplierRule, retryCfg RetryConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw := strings.TrimPrefix(req.RequestURI, "/")
		if raw == "" || !strings.Contains(raw, "://") {
			http.Error(w,
				"请把完整目标 URL 拼在路径里\n", http.StatusBadRequest)
			return
		}

		// 采集统计(在转发前抓取,因为 key/header 此时还在)。
		ip := clientIP(req)
		// 统计用 key 标签:key 注入模式用 statKeyLabel(如 "glm"),
		// 纯透传模式用掩码 key(如 "sk-****wxyz")。
		statKey := statKeyLabel
		if statKey == "" {
			statKey = maskedKeyFromRequest(req)
		}
		// 目标 host,用于统计(不含 path)。
		targetHost := hostFromRaw(raw)
		start := time.Now()

		// 用 statusRecorder 包一层,以便拿到最终状态码做统计。
		rec := &statusRecorder{ResponseWriter: w, status: 200}

		// WebSocket 分支:检测 Upgrade: websocket
		if isWebSocketUpgrade(req) {
			handleWebSocket(rec, req, raw)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{}, false)
			}
			return
		}

		// 普通 HTTP:原样转发
		// body 过滤(image_url → text):只对配置了 image_filter 规则的请求生效
		bodyReader := req.Body
		contentLength := req.ContentLength
		imageFiltered := false
		if len(imageFilter) > 0 {
			newBody, newLen, modified := filterImageBlocks(req.Body, imageFilter, targetHost)
			if newLen >= 0 {
				bodyReader = newBody
				contentLength = newLen
				imageFiltered = modified
			}
		}
		outReq, err := http.NewRequestWithContext(req.Context(), req.Method, raw, bodyReader)
		if err != nil {
			http.Error(rec, "目标 URL 无法解析: "+err.Error(), http.StatusBadRequest)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{}, false)
			}
			return
		}
		outReq.Header = req.Header.Clone() // 原样复制,不追加任何 header
		// key 注入模式:用配置的 key 覆盖对应 header(真实 key 不进日志/统计)
		if injectHeaders != nil {
			for k, vs := range injectHeaders {
				outReq.Header[k] = vs
			}
		}
		stripProxyHeaders(outReq.Header)     // 剥离上游网关注入的反代特征头,保持原始性
		outReq.ContentLength = contentLength // 显式带上 body 长度,避免 body 不被发送

		// 上游重试循环
		// 先把 body buffer 出来,每次重试都用 buffered body 重建请求。
		var bodyBuf []byte
		if outReq.Body != nil {
			bodyBuf, _ = io.ReadAll(outReq.Body)
			outReq.Body.Close()
		}
		// 用 buffered body 创建初始请求(替代已消耗的 outReq)
		if bodyBuf != nil {
			bodyReader := io.NopCloser(bytes.NewReader(bodyBuf))
			newReq, err := http.NewRequestWithContext(req.Context(), req.Method, raw, bodyReader)
			if err == nil {
				newReq.Header = outReq.Header.Clone()
				newReq.ContentLength = outReq.ContentLength
				outReq = newReq
			}
		}

		// 预计算 effective 配置(避免重复调用 effective() 导致 double-fill)
		eff := retryCfg.effective()
		maxAttempts := eff.MaxAttempts
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		var resp *http.Response
		var lastErr error
		model := ""
		// exhausted: 重试耗尽(最后一次响应仍是可重试码或连接错误)
		exhausted := false
	retryLoop:
		for attempt := 0; attempt < maxAttempts; attempt++ {
			// 重试时重建请求(首次就在上面建好了)
			if attempt > 0 {
				bodyReader := io.NopCloser(bytes.NewReader(bodyBuf))
				newReq, err := http.NewRequestWithContext(req.Context(), req.Method, raw, bodyReader)
				if err != nil {
					lastErr = err
					break
				}
				newReq.Header = outReq.Header.Clone()
				newReq.ContentLength = outReq.ContentLength
				outReq = newReq
			}

			resp, lastErr = sharedClient.Do(outReq)
			if lastErr != nil {
				// 客户端断连 → 不重试
				if errors.Is(lastErr, context.Canceled) {
					break
				}
				// 只有一次尝试(未配置重试)→ 直接走 502 路径
				if maxAttempts <= 1 {
					break
				}
				// 连接错误:已到最后一次 → 标记耗尽
				if attempt >= maxAttempts-1 {
					exhausted = true
					break
				}
				// 配置了重试 → 继续
				if !eff.RetryOnError {
					break
				}
				logRetry(statKeyLabel, model, attempt, maxAttempts, 0, lastErr)
				time.Sleep(eff.backoffDuration(attempt))
				continue
			}

			// 成功拿到响应 → 检查状态码
			if resp.StatusCode < 400 {
				// 成功 → 跳出循环,正常处理
				defer resp.Body.Close()
				break retryLoop
			}

			// 可重试状态码
			if eff.shouldRetryCode(resp.StatusCode) {
				resp.Body.Close()
				// 已到最后一次 → 标记耗尽
				if attempt >= maxAttempts-1 {
					exhausted = true
					resp = nil
					break
				}
				// 还有重试机会 → 退避后继续
				logRetry(statKeyLabel, model, attempt, maxAttempts, resp.StatusCode, nil)
				time.Sleep(eff.backoffDuration(attempt))
				continue
			}

			// 其他状态码(如 4xx) → 不重试,正常透传给客户端
			defer resp.Body.Close()
			break retryLoop
		}

		// 处理重试耗尽:返回 fallback_status 给客户端
		if exhausted {
			fallback := eff.FallbackStatus
			if fallback < 100 {
				fallback = http.StatusTooManyRequests // 默认 429
			}
			rec.Header().Set("Retry-After", "60")
			http.Error(rec, "上游服务暂时不可用,请稍后重试", fallback)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{}, false)
			}
			return
		}

		// 处理连接错误(非耗尽,不可重试的错误)
		if lastErr != nil {
			status := http.StatusBadGateway
			if errors.Is(lastErr, context.Canceled) {
				status = 499 // Client Closed Request(非标准,nginx 惯例)
				rec.status = status
			}
			http.Error(rec, "转发失败: "+lastErr.Error(), status)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{}, false)
			}
			return
		}

		dst := rec.Header()
		for k, vs := range resp.Header {
			dst[k] = vs // 响应头也原样透传
		}
		rec.WriteHeader(resp.StatusCode)

		// TTFB = 从请求开始到拿到响应头(首字节)
		ttfb := time.Since(start)

		// 检测是否为 SSE 流式响应(Content-Type: text/event-stream)
		isStream := isSSEResponse(resp.Header)

		// 流式转发:支持 SSE,边收边 flush。
		// 同时用 captureBuf 存一份,响应结束后异步解析 usage(不影响延迟)。
		//
		// 捕获策略:
		//   - 非 SSE:存全部 body(≤4MB,usage 在 JSON 顶层,需要完整解析)
		//   - SSE:用滑动窗口只保留最后 512KB + 第一个 chunk 的 model
		//     (usage 总在最后一个 chunk,model 在第一个 chunk,
		//      中间的增量内容不需要,这样即使 reasoning_content 有几十 MB 也不会截断 usage)
		var captureBuf []byte
		const maxCaptureFull = 4 * 1024 * 1024 // 非 SSE:最多 4MB
		const slidingWindow = 512 * 1024       // SSE:只保留最后 512KB
		var sseFirstChunk []byte               // SSE 第一个 chunk(含 model)

		if f, ok := rec.ResponseWriter.(http.Flusher); ok {
			buf := make([]byte, 32*1024)
			chunkCount := 0
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					rec.Write(buf[:n])
					f.Flush()
					if isStream {
						// SSE:第一个 chunk 存下来(含 model)
						if chunkCount == 0 {
							sseFirstChunk = append(sseFirstChunk, buf[:n]...)
						}
						// 滑动窗口:保留最后 512KB
						captureBuf = append(captureBuf, buf[:n]...)
						if len(captureBuf) > slidingWindow {
							captureBuf = captureBuf[len(captureBuf)-slidingWindow:]
						}
					} else {
						// 非 SSE:存全部(≤4MB)
						if len(captureBuf) < maxCaptureFull {
							captureBuf = append(captureBuf, buf[:n]...)
						}
					}
					chunkCount++
				}
				if rerr != nil {
					break
				}
			}
		} else {
			// 无 Flusher:用 TeeReader 在 copy 的同时捕获
			buf := make([]byte, 32*1024)
			chunkCount := 0
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					rec.Write(buf[:n])
					if isStream {
						if chunkCount == 0 {
							sseFirstChunk = append(sseFirstChunk, buf[:n]...)
						}
						captureBuf = append(captureBuf, buf[:n]...)
						if len(captureBuf) > slidingWindow {
							captureBuf = captureBuf[len(captureBuf)-slidingWindow:]
						}
					} else {
						if len(captureBuf) < maxCaptureFull {
							captureBuf = append(captureBuf, buf[:n]...)
						}
					}
					chunkCount++
				}
				if rerr != nil {
					break
				}
			}
		}

		// SSE:把第一个 chunk(含 model)拼到 captureBuf 前面,
		// 这样 extractUsage 既能拿到 model 又能拿到尾部 usage
		if isStream && len(sseFirstChunk) > 0 {
			captureBuf = append(sseFirstChunk, captureBuf...)
		}

		// 解析 token 用量(此时响应已全部转发给客户端,不增加延迟)。
		// 别名模式(key:xxx)和透传模式(客户端自带 key)都提取。
		// 透传模式下用掩码 key(如 sk-abcd****5678)作为统计分组。
		var u usageData
		if len(captureBuf) > 0 {
			u = extractUsage(captureBuf)
			// DEBUG: SSE 场景下 model 提取诊断
			if isStream && u.HasData && u.Model == "" {
				head := captureBuf
				if len(head) > 150 {
					head = head[:150]
				}
				log.Printf("SSE model 提取失败: bufLen=%d sseFirstChunkLen=%d prompt=%d head=%q",
					len(captureBuf), len(sseFirstChunk), u.Prompt, string(head))
			}
			if u.HasData && usageTracker != nil {
				// 尝试用 cost 计算费用（混合计费：未命中按标准价 + 命中按优惠价）
				c, err := cost.Calculate(u.Model, int(u.Prompt), int(u.Completion), int(u.Cached))
				if err == nil {
					u.CostCalculated = true
					u.InputCost = c.InputCost
					u.OutputCost = c.OutputCost
					u.TotalCost = c.TotalCost
				} else {
					// 费用算不出(模型不在定价表),记录模型名方便排查该配哪个模型
					log.Printf("费用计算失败(模型不在定价表): model=%q alias=%s prompt=%d", u.Model, statKey, u.Prompt)
				}
				// 即使费用计算失败(模型不在定价表),也照常记录 token 用量

				// Token 用量乘数: 按模型名+域名匹配,乘数作用于 Prompt/Cached/Completion 和费用
				if len(tokenMultipliers) > 0 && u.HasData {
					m := applyTokenMultiplier(tokenMultipliers, u.Model, targetHost)
					if m != 1.0 {
						u.Multiplier = m
						u.Prompt = int64(float64(u.Prompt) * m)
						u.Cached = int64(float64(u.Cached) * m)
						u.Completion = int64(float64(u.Completion) * m)
						u.InputCost *= m
						u.OutputCost *= m
						u.TotalCost *= m
						log.Printf("用量乘数: alias=%s model=%q host=%s multiplier=%.2f prompt=%d cached=%d completion=%d cost=%.6f",
							statKey, u.Model, targetHost, m, u.Prompt, u.Cached, u.Completion, u.TotalCost)
					}
				}

				// 统计分组:别名模式用别名,透传模式用掩码 key
				alias := statKey
				if strings.HasPrefix(statKey, "key:") {
					alias = strings.TrimPrefix(statKey, "key:")
				}
				// statKey 为 "-" (无 key 的请求)不纳入 usage 统计
				if alias != "-" {
					usageTracker.record(alias, u)
				}
			}
		}

		// 记录统计(含 token 用量)
		if stats != nil {
			stats.record(ip, statKey, targetHost, rec.status)
		}
		// 错误请求(4xx/5xx)记录到 usage 统计
		if usageTracker != nil && rec.status >= 400 {
			alias := statKey
			if strings.HasPrefix(statKey, "key:") {
				alias = strings.TrimPrefix(statKey, "key:")
			}
			usageTracker.recordError(alias)
		} else if usageTracker != nil && rec.status >= 200 && rec.status < 400 {
			// 成功请求(2xx/3xx)记录到窗口计数器(用于用量限额)
			alias := statKey
			if strings.HasPrefix(statKey, "key:") {
				alias = strings.TrimPrefix(statKey, "key:")
			}
			usageTracker.recordSuccess(alias)
		}
		logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), ttfb, isStream, u, imageFiltered)
	})
}

// statusRecorder 包装 ResponseWriter 以捕获状态码。
// 必须转发 http.Hijacker / http.Flusher 接口,否则 WebSocket 和 SSE 会失败。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack 转发给底层 ResponseWriter,保证 WebSocket 可用。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("ResponseWriter 不支持 Hijack")
	}
	return hj.Hijack()
}

// Flush 转发给底层 ResponseWriter,保证 SSE 可用。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// extractDomain 从目标 URL "https://api.z.ai/path" 提取域名(小写,去端口)。
// 用于域名白名单检查。
func extractDomain(target string) string {
	// target 形如 "https://api.z.ai:8443/path"
	rest := target
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	// 去掉 path,只留 host:port
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	// 去掉端口
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest)
}

// pickHeader 根据 KeyConfig + 客户端请求的 header 决定注入哪个 header、值是什么。
// 规则(简洁直接):
//  1. KeyConfig 显式配了 Header(向后兼容) → 用配的,prefix 按 cfg.Prefix 或 Bearer 默认
//  2. 自动模式:客户端带了哪个 header 就替换哪个:
//     - 带 x-api-key → 替换成 x-api-key: {key}(无前缀)
//     - 带 Authorization → 替换成 Authorization: Bearer {key}
//     - 都带了 → 两个都替换(同时注入)
//     - 都没带 → 不注入(返回空),客户端用自己的(通常会被上游拒)
func pickHeader(cfg KeyConfig, clientHeaders http.Header) http.Header {
	// 显式配置优先(向后兼容老配置)
	if cfg.Header != "" {
		h := http.Header{}
		val := cfg.Key
		if cfg.Prefix != "" {
			val = cfg.Prefix + cfg.Key
		} else if cfg.Header == "Authorization" {
			val = "Bearer " + cfg.Key
		}
		h.Set(cfg.Header, val)
		return h
	}

	// 自动模式:客户端带了什么就替换什么
	inject := http.Header{}
	if clientHeaders.Get("X-Api-Key") != "" {
		inject.Set("x-api-key", cfg.Key) // x-api-key 不需要 Bearer 前缀
	}
	if clientHeaders.Get("Authorization") != "" {
		inject.Set("Authorization", "Bearer "+cfg.Key)
	}
	return inject // 可能为空(都没带 → 不注入)
}

// stripProxyHeaders 剥离上游反向代理/网关可能注入的"指纹"头,
// 让转发出去的请求保持客户端原始样貌,避免被目标 API 发现经过了中间层。
// 剥离范围: X-Forwarded-* / Via / X-Real-IP / X-Request-ID / X-Forwarded-Proto 等。
func stripProxyHeaders(h http.Header) {
	for k := range h {
		lk := strings.ToLower(k)
		// X-Forwarded-* 系列(含 Scheme/For/Proto/Host/Server 等)
		if strings.HasPrefix(lk, "x-forwarded-") {
			delete(h, k)
			continue
		}
		// 其他常见反代指纹头
		switch lk {
		case "via", "x-real-ip", "x-request-id", "x-requested-with",
			"x-original-url", "x-rewrite-url", "x-nginx-proxy",
			"true-client-ip", "cf-connecting-ip", "cf-ipcountry",
			"cf-ray", "cf-visitor":
			delete(h, k)
		}
		// Accept-Encoding: 强制剥离,让上游返回明文(不压缩)。
		// 否则客户端带了 gzip 时,上游会返回 gzip 压缩的 SSE 流,
		// 导致 captureBuf 里是二进制压缩数据,extractUsage 解不出 usage/token/费用。
		// 副作用:SSE 流式不再被 gzip 缓冲,实时性反而更好。
		if lk == "accept-encoding" {
			delete(h, k)
		}
	}
}

// hostFromRaw 从原始目标 URL 字符串里提取 host(不含端口/路径),用于统计。
func hostFromRaw(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "-"
	}
	return u.Host
}

// isWebSocketUpgrade 判断是否为 WebSocket 升级请求。
func isWebSocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// isSSEResponse 检查响应头是否为 SSE 流式响应。
// SSE 响应的 Content-Type 为 text/event-stream。
func isSSEResponse(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// handleWebSocket 把 WebSocket 连接作为原始 TCP 双向隧道转发。
// 不解析 WS 帧协议,直接在底层 TCP 上两个方向同时拷贝字节流 ——
// 这样对 WS 子协议(普通/加密/自定义)全部透明兼容。
func handleWebSocket(w http.ResponseWriter, req *http.Request, targetURL string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "服务器不支持 WebSocket(无法 hijack)", http.StatusInternalServerError)
		return
	}

	// 解析目标 URL,取 host:port 和 path+query。
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" {
		http.Error(w, "目标 URL 无法解析", http.StatusBadRequest)
		return
	}
	hostport := hostFromURL(targetURL)
	useTLS := strings.HasPrefix(targetURL, "wss://") || strings.HasPrefix(targetURL, "https://")

	// 连接上游
	var backend net.Conn
	if useTLS {
		backend, err = tlsDialWithServerName(hostport)
	} else {
		backend, err = net.DialTimeout("tcp", hostport, 30*time.Second)
	}
	if err != nil {
		http.Error(w, "连接上游失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()

	// hijack 到底层 TCP
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// 手动构造握手请求行+头,避免 req.Write 用被规范化的 URL。
	// 请求行的 path 必须用原始 RequestURI 里的(去掉目标 host 前缀后的剩余部分)。
	// targetURL 形如 wss://ws.postman-echo.com/raw
	// 客户端原始 RequestURI 形如 /wss://ws.postman-echo.com/raw
	// 我们需要的 path 是 u.RequestURI()(= "/raw")。
	requestPath := u.RequestURI()

	// 写请求行
	fmt.Fprintf(backend, "%s %s HTTP/1.1\r\n", req.Method, requestPath)
	// 写 Host(用上游真实 host)
	fmt.Fprintf(backend, "Host: %s\r\n", u.Host)
	// 原样转发其余 header(除 Host 外,因为 Host 上面已写)
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(backend, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(backend, "\r\n")

	// 把 hijack bufio 里已缓冲的客户端数据先冲给上游
	if clientBuf != nil {
		if n := clientBuf.Reader.Buffered(); n > 0 {
			io.Copy(backend, clientBuf)
		}
	}

	// 双向隧道:两个方向同时拷贝字节流
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(clientConn, backend)
		if tcp, ok := clientConn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(backend, clientConn)
		if tcp, ok := backend.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	wg.Wait()
}
