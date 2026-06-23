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
	"crypto/tls"
	"encoding/json"
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

	// allowDomains 是全局域名白名单(key 注入模式下只允许代理到这些域名)。
	// 防止有人把请求指向自己的网站,利用代理注入的真实 key 造成泄露。
	// 为空时表示不限制(仅限内网/受信环境)。
	allowDomains map[string]bool

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
	allowSrc := *allowDom
	if allowSrc == "" {
		allowSrc = os.Getenv("ALLOW_DOMAINS")
	}
	if allowSrc != "" {
		allowDomains = make(map[string]bool)
		for _, d := range strings.Split(allowSrc, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			if d != "" {
				allowDomains[d] = true
			}
		}
		log.Printf("域名白名单已启用: %d 个域名", len(allowDomains))
	}

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
		admin = newAdminServer(adminPassword, stats, ks, quotaCacheInst, usageTracker)
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
			versionHandler(w, req)
			return
		case req.URL.Path == "/__stats":
			// 统计端点鉴权:如果启用了管理界面,需登录才能看
			var authFn func(*http.Request) bool
			if admin != nil {
				authFn = admin.authCheck
			}
			statsHandler(stats, authFn).ServeHTTP(w, req)
			return
		case ks != nil && strings.HasPrefix(req.URL.Path, "/k/"):
			// key 注入模式:/k/{alias}/https://目标
			handleKeyRoute(w, req, ks, stats)
			return
		default:
			// 纯透传模式:/https://目标
			newProxyHandler(stats, nil, "").ServeHTTP(w, req)
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

// handleKeyRoute 处理 key 注入模式: /k/{alias}/https://目标
// 从 keyStore 查 alias 配置,限流检查,注入真实 key 到 header,然后走转发。
// 用户不需要带 key(带了也会被配置的 key 覆盖)。
func handleKeyRoute(w http.ResponseWriter, req *http.Request, ks *keyStore, stats *statsCollector) {
	// 原始 RequestURI 形如: /k/glm/https://open.bigmodel.cn/api/...
	// 去掉前导 / 得: k/glm/https://open.bigmodel.cn/api/...
	raw := strings.TrimPrefix(req.RequestURI, "/")

	// 解析 k/{alias}/{target}
	// 先去掉 "k/" 前缀
	rest := strings.TrimPrefix(raw, "k/")
	if rest == raw {
		http.Error(w, "路径格式应为 /k/{alias}/https://目标\n", http.StatusBadRequest)
		return
	}
	// 取第一个 / 之前的部分作为 alias
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "缺少目标 URL,格式应为 /k/{alias}/https://目标\n", http.StatusBadRequest)
		return
	}
	alias := rest[:slash]
	target := rest[slash+1:]
	if !strings.Contains(target, "://") {
		http.Error(w, "目标 URL 需以 http:// 或 https:// 开头\n", http.StatusBadRequest)
		return
	}

	// 查 alias 配置(lookup 内含过期检查)
	cfg, ok := ks.lookup(alias)
	if !ok {
		// 区分"不存在"和"已过期"
		if ks.isExpired(alias) {
			http.Error(w, "此 key 标识已过期: "+alias+"\n", http.StatusGone)
		} else {
			http.Error(w, "未知的 key 标识: "+alias+"\n", http.StatusNotFound)
		}
		return
	}

	// 限流检查
	if !ks.allow(alias) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "请求过于频繁,请稍后重试 (alias: "+alias+")\n", http.StatusTooManyRequests)
		return
	}

	// 域名白名单检查:提取目标域名,不在白名单内则拒绝(防 key 泄露)
	targetDomain := extractDomain(target)
	if len(allowDomains) > 0 && !allowDomains[targetDomain] {
		log.Printf("拒绝代理: 目标域名 %q 不在白名单 (alias=%s)", targetDomain, alias)
		http.Error(w, "目标域名不在白名单: "+targetDomain+"\n", http.StatusForbidden)
		return
	}

	// 构造注入 header(自动模式:客户端带了什么就替换什么)
	inject := pickHeader(cfg, req.Header)
	if len(inject) == 0 {
		// 客户端没带 x-api-key 也没带 Authorization → 不注入,直接透传
		// (上游会用客户端自己的 header,通常没 key 会被拒)
	}

	// 重写 RequestURI 让 newProxyHandler 解析出正确的 target。
	// 直接改原 req(本函数之后不再用它),避免 Clone 不复制 Body 导致 POST body 丢失。
	// 注意: http.Request.Clone 文档明确 Body 不会被克隆(io.Reader 不可复制),
	// 所以 GET(无 body)能过但 POST 会丢 body。直接改原 req 最可靠。
	req.RequestURI = "/" + target
	// 统计标签用 alias(如 "glm"),不暴露真实 key
	statLabel := "key:" + alias

	newProxyHandler(stats, inject, statLabel).ServeHTTP(w, req)
}

// newProxyHandler 构造代理的 http.Handler。测试和 main 共用同一份逻辑。
// stats 非 nil 时,记录每次请求的 IP/掩码key/状态码统计。
// injectHeaders 非 nil 时(key 注入模式),这些 header 会在转发前覆盖进请求头。
// statKeyLabel 用于统计里替代掩码 key 显示(如 key 注入模式显示 alias "glm")。
func newProxyHandler(stats *statsCollector, injectHeaders http.Header, statKeyLabel string) http.Handler {
	transport := &http.Transport{
		DisableCompression: true, // 不偷偷加 Accept-Encoding: gzip
	}
	client := &http.Client{Transport: transport}

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
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{})
			}
			return
		}

		// 普通 HTTP:原样转发
		outReq, err := http.NewRequestWithContext(req.Context(), req.Method, raw, req.Body)
		if err != nil {
			http.Error(rec, "目标 URL 无法解析: "+err.Error(), http.StatusBadRequest)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{})
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
		stripProxyHeaders(outReq.Header)         // 剥离上游网关注入的反代特征头,保持原始性
		outReq.ContentLength = req.ContentLength // 显式带上 body 长度,避免 body 不被发送

		resp, err := client.Do(outReq)
		if err != nil {
			http.Error(rec, "转发失败: "+err.Error(), http.StatusBadGateway)
			if stats != nil {
				stats.record(ip, statKey, targetHost, rec.status)
				logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), 0, false, usageData{})
			}
			return
		}
		defer resp.Body.Close()

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
			if u.HasData && usageTracker != nil {
				// 尝试用 cost 计算费用（混合计费：未命中按标准价 + 命中按优惠价）
				c, err := cost.Calculate(u.Model, int(u.Prompt), int(u.Completion), int(u.Cached))
				if err == nil {
					u.CostCalculated = true
					u.InputCost = c.InputCost
					u.OutputCost = c.OutputCost
					u.TotalCost = c.TotalCost
				}
				// 即使费用计算失败(模型不在定价表),也照常记录 token 用量

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
		}
		logRequest(ip, statKey, req.Method, targetHost, rec.status, time.Since(start), ttfb, isStream, u)
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
