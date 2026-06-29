// admin.go — Web 管理界面(服务端渲染 + 单一密码鉴权)
//
// 端点:
//   /__admin          dashboard(版本 + 统计概览)
//   /__admin/login    登录(GET 显示表单,POST 校验密码)
//   /__admin/logout   登出
//   /__admin/keys     管理 keys.yaml(查看/新增/删除)
//   /__admin/stats    详细统计
//   /__admin/logs     最近请求日志
//
// 鉴权:单一密码(ADMIN_PASSWORD),登录后设 HMAC 签名 cookie(24h 有效)。
// 用 crypto/subtle.ConstantTimeCompare 防时序攻击。

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// cookieName 是登录态 cookie 的名字。
const cookieName = "lhp_admin"
const cookieTTL = 30 * 24 * time.Hour // 30 天,不用频繁登录

// adminServer 是管理界面的核心,持有所有依赖。
type adminServer struct {
	password string // 登录密码
	secret   []byte // HMAC 签名密钥(进程启动时随机生成)
	stats    *statsCollector
	keys     *keyStore        // 可能为 nil(未启用 -keys 时)
	quota    *quotaCache      // 可能为 nil(未启用 -keys 时)
	usage    *usageStats      // 可能为 nil(全局,直接用全局变量也行)
	settings *settingsManager // 运行时设置管理器
	// 启动参数(只读展示)
	startupAddr     string
	startupPersist  string
	startupKeys     string
	startupAdminPwd bool
}

// newAdminServer 创建管理界面。password 为空则不启用(返回 nil)。
func newAdminServer(password string, stats *statsCollector, ks *keyStore, qc *quotaCache, us *usageStats, sm *settingsManager, addr, persist, keys string) *adminServer {
	if password == "" {
		return nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		log.Fatalf("生成 HMAC 密钥失败: %v", err)
	}
	return &adminServer{
		password:        password,
		secret:          secret,
		stats:           stats,
		keys:            ks,
		quota:           qc,
		usage:           us,
		settings:        sm,
		startupAddr:     addr,
		startupPersist:  persist,
		startupKeys:     keys,
		startupAdminPwd: password != "",
	}
}

// --- 鉴权 ---

// makeCookieValue 生成签名 cookie 值:hex(timestamp) + "." + hex(hmac(timestamp))。
func (a *adminServer) makeCookieValue() string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(ts))
	sig := hex.EncodeToString(mac.Sum(nil))
	return ts + "." + sig
}

// validCookie 校验 cookie 值是否有效(签名正确 + 未过期)。
func (a *adminServer) validCookie(val string) bool {
	parts := strings.SplitN(val, ".", 2)
	if len(parts) != 2 {
		return false
	}
	tsStr, sigHex := parts[0], parts[1]
	// 校验签名(常数时间比较防时序攻击)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(tsStr))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sigHex), []byte(expectedSig)) != 1 {
		return false
	}
	// 校验时间戳(未过期)
	var ts int64
	fmt.Sscanf(tsStr, "%d", &ts)
	if ts <= 0 {
		return false
	}
	age := time.Since(time.Unix(ts, 0))
	if age > cookieTTL || age < -time.Minute {
		return false
	}
	return true
}

// isLoggedIn 检查请求是否已登录(从 cookie 判断)。
func (a *adminServer) isLoggedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return a.validCookie(c.Value)
}

// authCheck 是给 statsHandler 用的鉴权函数(实现 func(*http.Request) bool)。
func (a *adminServer) authCheck(r *http.Request) bool {
	return a.isLoggedIn(r)
}

// requireAuth 是中间件:未登录跳转登录页。
func (a *adminServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isLoggedIn(r) {
			http.Redirect(w, r, "/__admin/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// --- 路由 ---

// handler 返回管理界面的 http.Handler(挂在 /__admin 下)。
func (a *adminServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/__admin", a.requireAuth(a.handleDashboard))
	mux.HandleFunc("/__admin/", a.handleAdminSub)
	mux.HandleFunc("/__admin/login", a.handleLogin)
	mux.HandleFunc("/__admin/logout", a.handleLogout)
	mux.HandleFunc("/__admin/keys", a.requireAuth(a.handleKeys))
	mux.HandleFunc("/__admin/keys/new", a.requireAuth(a.handleKeyNew))
	mux.HandleFunc("/__admin/keys/delete", a.requireAuth(a.handleKeyDelete))
	mux.HandleFunc("/__admin/stats", a.requireAuth(a.handleStats))
	mux.HandleFunc("/__admin/logs", a.requireAuth(a.handleLogs))
	mux.HandleFunc("/__admin/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("/__admin/config", a.requireAuth(a.handleConfig))
	mux.HandleFunc("/__admin/profiles", a.requireAuth(a.handleProfiles))
	mux.HandleFunc("/__admin/profiles/new", a.requireAuth(a.handleProfileNew))
	mux.HandleFunc("/__admin/profiles/delete", a.requireAuth(a.handleProfileDelete))
	mux.HandleFunc("/__admin/quota/refresh", a.requireAuth(a.handleQuotaRefresh))
	return mux
}

// handleAdminSub 处理 /__admin/ 下的子路径分发(已被 mux 精确匹配兜底)。
func (a *adminServer) handleAdminSub(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__admin/" {
		a.requireAuth(a.handleDashboard)(w, r)
		return
	}
	http.NotFound(w, r)
}

// --- 登录/登出 ---

func (a *adminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		renderLogin(w)
		return
	}
	if r.Method == http.MethodPost {
		r.ParseForm()
		pw := r.FormValue("password")
		// 常数时间比较防时序攻击
		if subtle.ConstantTimeCompare([]byte(pw), []byte(a.password)) == 1 {
			// HTTPS 请求时给 cookie 加 Secure 标志,防止明文链路截获。
			// 开发环境(HTTP)不加,否则 cookie 不生效。
			secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    a.makeCookieValue(),
				Path:     "/",
				HttpOnly: true,
				Secure:   secure,
				MaxAge:   int(cookieTTL.Seconds()),
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/__admin", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		renderLogin(w)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (a *adminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, Secure: secure,
	})
	http.Redirect(w, r, "/__admin/login", http.StatusSeeOther)
}

// --- Dashboard ---

func (a *adminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snap := a.stats.snapshot()
	data := struct {
		Version   string
		BuildTime string
		StartTime string
		Uptime    string
		TotalIPs  int
		TotalReq  int64
		KeysCount int
		QuotaHTML template.HTML // 配额展示(预渲染 HTML,可能为空)
		UsageHTML template.HTML // token 用量统计(预渲染 HTML,可能为空)
	}{
		Version:   version,
		BuildTime: buildTime,
		StartTime: startTime.In(beijing).Format("2006-01-02 15:04:05"),
		Uptime:    time.Since(startTime).Round(time.Second).String(),
		TotalIPs:  len(snap),
		TotalReq:  totalCount(snap),
	}
	if a.keys != nil {
		data.KeysCount = len(a.keys.allConfigs())
	}
	if a.quota != nil {
		entries := a.quota.getAll()
		if html := buildQuotaHTML(entries); html != "" {
			data.QuotaHTML = template.HTML(html)
		}
	}
	if a.usage != nil {
		keysCfg := a.keys.allConfigs() // 传 key 配置可展示限额列
		if html := buildUsageHTML(a.usage.snapshot(), keysCfg); html != "" {
			data.UsageHTML = template.HTML(html)
		}
	}
	renderTemplate(w, "dashboard", data)
}

// handleQuotaRefresh 手动触发配额立即刷新,刷新完跳回 Dashboard。
// 解决"刚加完 key 要等 5 分钟才看到配额"的问题。
func (a *adminServer) handleQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if a.quota == nil {
		renderMsg(w, "配额刷新不可用", "启动时需加 -keys 参数才能拉取配额。")
		return
	}
	n := a.quota.refreshNow(a.keys)
	renderMsg(w, "配额已刷新",
		fmt.Sprintf("已重新拉取 %d 个 key 的配额数据,返回 Dashboard 查看。", n))
}

// --- Profiles 管理(拦截器模板) ---

func (a *adminServer) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		renderMsg(w, "Key 注入模式未启用", "启动时需加 -keys 参数。")
		return
	}
	editName := r.URL.Query().Get("edit")
	copyName := r.URL.Query().Get("copy")
	var editProf InterceptorProfile
	editing := false
	copying := false
	if editName != "" {
		if profiles := a.keys.allProfiles(); profiles != nil {
			if p, ok := profiles[editName]; ok {
				editProf = p
				editing = true
			}
		}
	} else if copyName != "" {
		if profiles := a.keys.allProfiles(); profiles != nil {
			if p, ok := profiles[copyName]; ok {
				editProf = p
				copying = true
			}
		}
	}
	data := struct {
		Profiles     map[string]InterceptorProfile
		EditName     string
		EditProf     InterceptorProfile
		Editing      bool
		Copying      bool
		CopyFrom     string
		HasTimeBlock bool
	}{
		Profiles:     a.keys.allProfiles(),
		EditName:     editName,
		EditProf:     editProf,
		Editing:      editing,
		Copying:      copying,
		CopyFrom:     copyName,
		HasTimeBlock: editProf.TimeBlock != nil,
	}
	renderTemplate(w, "profiles", data)
}

func (a *adminServer) handleProfileNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		http.Error(w, "key 模式未启用", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	rate, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("rate")))
	burst, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("burst")))
	maxTokens, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("max_tokens")), 10, 64)
	maxReqs, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("max_requests")), 10, 64)
	window := strings.TrimSpace(r.FormValue("window"))
	tbStart := strings.TrimSpace(r.FormValue("time_block_start"))
	tbEnd := strings.TrimSpace(r.FormValue("time_block_end"))

	if name == "" {
		renderMsg(w, "错误", "名称不能为空")
		return
	}
	if rate < 0 {
		rate = 0
	}
	if burst < 0 {
		burst = 0
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	if maxReqs < 0 {
		maxReqs = 0
	}

	profile := InterceptorProfile{
		Rate:      rate,
		Burst:     burst,
		MaxTokens: maxTokens,
		MaxReqs:   maxReqs,
		Window:    window,
	}
	if tbStart != "" || tbEnd != "" {
		profile.TimeBlock = &TimeBlock{Start: tbStart, End: tbEnd}
	}

	if err := a.keys.setProfile(name, profile); err != nil {
		renderMsg(w, "保存失败", err.Error())
		return
	}
	http.Redirect(w, r, "/__admin/profiles", http.StatusSeeOther)
}

func (a *adminServer) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		http.Error(w, "key 模式未启用", http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	if err := a.keys.deleteProfile(name); err != nil {
		renderMsg(w, "删除失败", err.Error())
		return
	}
	http.Redirect(w, r, "/__admin/profiles", http.StatusSeeOther)
}

// --- Keys 管理 ---

func (a *adminServer) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		renderMsg(w, "Key 注入模式未启用", "启动时需加 -keys 参数才能管理 key 配置。")
		return
	}
	// ?edit=alias:回填配置,alias 只读(改其他字段)
	// ?copy=alias:回填配置,但 alias 留空可改(基于此配置新建另一个 alias)
	// 用相对路径(用户已知自己的代理地址,不用从请求推断)
	editAlias := r.URL.Query().Get("edit")
	copyAlias := r.URL.Query().Get("copy")
	var editCfg KeyConfig
	editing := false
	copying := false
	if editAlias != "" {
		if cfg, ok := a.keys.allConfigs()[editAlias]; ok {
			editCfg = cfg
			editing = true
		}
	} else if copyAlias != "" {
		if cfg, ok := a.keys.allConfigs()[copyAlias]; ok {
			editCfg = cfg
			copying = true
		}
	}
	data := struct {
		Aliases   map[string]KeyConfig
		Expired   map[string]bool
		BasePath  string
		EditAlias string
		EditCfg   KeyConfig
		Editing   bool
		Copying   bool
		CopyFrom  string
	}{
		Aliases:   a.keys.allConfigs(),
		Expired:   a.keys.expiredMap(),
		BasePath:  "/k/",
		EditAlias: editAlias,
		EditCfg:   editCfg,
		Editing:   editing,
		Copying:   copying,
		CopyFrom:  copyAlias,
	}
	renderTemplate(w, "keys", data)
}

func (a *adminServer) handleKeyNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		http.Error(w, "key 模式未启用", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	alias := strings.TrimSpace(r.FormValue("alias"))
	key := strings.TrimSpace(r.FormValue("key"))
	header := strings.TrimSpace(r.FormValue("header"))
	// prefix 不 TrimSpace(保留尾空格,如 "Bearer "),但去掉首空格
	prefix := strings.TrimLeft(r.FormValue("prefix"), " ")
	rate, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("rate")))
	burst, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("burst")))
	if rate < 0 {
		rate = 0
	}
	if burst < 0 {
		burst = 0
	}
	expires := normalizeExpires(r.FormValue("expires"))

	if alias == "" {
		renderMsg(w, "错误", "alias 不能为空")
		return
	}
	// 编辑模式:alias 已存在且 key 留空 → 保留原 key
	if key == "" {
		if existing, ok := a.keys.allConfigs()[alias]; ok {
			key = existing.Key
		}
	}
	if key == "" {
		renderMsg(w, "错误", "key 不能为空")
		return
	}
	// header 为空 = 自动检测模式(推荐),不强制默认。
	// 只有显式选了 Authorization 才做 Bearer 智能修正。
	if header == "Authorization" && prefix == "Bearer" {
		prefix = "Bearer "
	}
	// 校验有效期格式(空=永久)。支持 "YYYY-MM-DD" 和 "YYYY-MM-DD HH:MM[:SS]"
	if expires != "" {
		if _, ok := parseExpires(expires); !ok {
			renderMsg(w, "有效期格式错误", `请用 "YYYY-MM-DD"(到当天结束)或 "YYYY-MM-DD HH:MM"(精确到分,北京时间)格式,或留空表示永久。`)
			return
		}
	}
	cfg := KeyConfig{Key: key, Header: header, Prefix: prefix, Rate: rate, Burst: burst, Expires: expires}
	if err := a.keys.setConfig(alias, cfg); err != nil {
		renderMsg(w, "保存失败", err.Error())
		return
	}
	http.Redirect(w, r, "/__admin/keys", http.StatusSeeOther)
}

func (a *adminServer) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.keys == nil {
		http.Error(w, "key 模式未启用", http.StatusBadRequest)
		return
	}
	alias := r.URL.Query().Get("alias")
	if alias == "" {
		http.Error(w, "缺少 alias", http.StatusBadRequest)
		return
	}
	if err := a.keys.deleteConfig(alias); err != nil {
		renderMsg(w, "删除失败", err.Error())
		return
	}
	http.Redirect(w, r, "/__admin/keys", http.StatusSeeOther)
}

// --- Stats ---

func (a *adminServer) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := a.stats.snapshot()
	byIP := statsByIP(snap)
	renderTemplate(w, "stats", byIP)
}

// --- Logs ---

func (a *adminServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	entries := globalLogRing.recent(200)
	renderTemplate(w, "logs", entries)
}

// --- Settings ---

func (a *adminServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		action := r.FormValue("action")
		switch action {
		case "add":
			domain := strings.TrimSpace(r.FormValue("domain"))
			if domain == "" {
				renderMsg(w, "无效输入", "域名不能为空")
				return
			}
			a.settings.AddDomain(domain)
			http.Redirect(w, r, "/__admin/settings", http.StatusSeeOther)
			return
		case "remove":
			domain := strings.TrimSpace(r.FormValue("domain"))
			if domain != "" {
				a.settings.RemoveDomain(domain)
			}
			http.Redirect(w, r, "/__admin/settings", http.StatusSeeOther)
			return
		default:
			renderMsg(w, "未知操作", "仅支持 add/remove")
			return
		}
	}

	// GET: 渲染设置页面
	domains := a.settings.GetDomains()
	whitelistEnabled := a.settings.IsWhitelistEnabled()

	data := struct {
		Domains          []string
		WhitelistEnabled bool
		DomainCount      int
		Addr             string
		Persist          string
		Keys             string
		AdminEnabled     bool
	}{
		Domains:          domains,
		WhitelistEnabled: whitelistEnabled,
		DomainCount:      len(domains),
		Addr:             a.startupAddr,
		Persist:          a.startupPersist,
		Keys:             a.startupKeys,
		AdminEnabled:     a.startupAdminPwd,
	}
	renderTemplate(w, "settings", data)
}

// --- Config (YAML 编辑器) ---

func (a *adminServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if a.keys == nil {
		renderMsg(w, "Key 注入模式未启用", "启动时需加 -keys 参数才能编辑配置。")
		return
	}
	// 路径可能为空(首次启动,还没保存过)
	path := a.keys.getPath()

	if r.Method == http.MethodPost {
		r.ParseForm()
		raw := r.FormValue("yaml")
		if raw == "" {
			renderConfig(w, "YAML 内容不能为空", raw, path)
			return
		}
		// 校验 YAML 语法 + 结构完整性(和 load() 使用的相同逻辑)
		if err := validateYAML([]byte(raw)); err != nil {
			renderConfig(w, "YAML 语法错误: "+err.Error(), raw, path)
			return
		}
		// 原子写回文件(tmp+rename)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(raw), 0600); err != nil {
			renderConfig(w, "写入失败: "+err.Error(), raw, path)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			renderConfig(w, "重命名失败: "+err.Error(), raw, path)
			return
		}
		// 文件已变,reloadIfChanged 会在下次 tick 自动加载。
		// 但为了即时生效,主动 reload 一次。
		a.keys.forceReload()
		renderMsg(w, "配置已保存", "YAML 配置已保存并重新加载。")
		return
	}

	// GET: 读取当前文件内容
	var current string
	if data, err := os.ReadFile(path); err == nil {
		current = string(data)
	}
	renderConfig(w, "", current, path)
}

// renderConfig 渲染 YAML 配置页(错误时保留编辑内容)。
func renderConfig(w http.ResponseWriter, errMsg, rawYAML, path string) {
	// 错误信息和 YAML 内容从 textarea 里拿
	data := struct {
		Error string
		YAML  string
		Path  string
	}{
		Error: errMsg,
		YAML:  rawYAML,
		Path:  path,
	}
	renderTemplate(w, "config", data)
}

// validateYAML 校验 YAML 语法和结构,和 keyStore.load() 使用相同逻辑。
// 先解析为原始 map,然后尝试将各个段解析为实际类型。
func validateYAML(data []byte) error {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	// 尝试解析各个已知段
	if filterRaw, ok := raw["image_filter"]; ok {
		filterData, _ := yaml.Marshal(filterRaw)
		var rules []ImageFilterRule
		if err := yaml.Unmarshal(filterData, &rules); err != nil {
			return fmt.Errorf("image_filter 段: %w", err)
		}
	}
	if multRaw, ok := raw["token_multipliers"]; ok {
		multData, _ := yaml.Marshal(multRaw)
		var multipliers []TokenMultiplierRule
		if err := yaml.Unmarshal(multData, &multipliers); err != nil {
			return fmt.Errorf("token_multipliers 段: %w", err)
		}
	}
	if profRaw, ok := raw["interceptor_profiles"]; ok {
		profData, _ := yaml.Marshal(profRaw)
		var profiles map[string]InterceptorProfile
		if err := yaml.Unmarshal(profData, &profiles); err != nil {
			return fmt.Errorf("interceptor_profiles 段: %w", err)
		}
	}
	// 去掉已知全局段,剩余部分作为 keys 解析
	delete(raw, "image_filter")
	delete(raw, "token_multipliers")
	delete(raw, "interceptor_profiles")
	keysData, _ := yaml.Marshal(raw)
	var configs map[string]KeyConfig
	if err := yaml.Unmarshal(keysData, &configs); err != nil {
		return fmt.Errorf("alias 配置段: %w", err)
	}
	return nil
}

// --- 模板渲染 ---

func renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	tmpl := template.New(name).Funcs(template.FuncMap{
		"mul100": func(v float64) float64 { return v * 100 },
		"divf": func(a, b int64) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b)
		},
		"mul":       func(a, b float64) float64 { return a * b },
		"fmtTokens": func(n int64) string { return fmtTokens(n) },
	})
	// 先解析公共片段(head/nav),再解析页面模板
	if _, err := tmpl.Parse(baseTemplates); err != nil {
		http.Error(w, "模板基础错误: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tmpl.Parse(adminTemplates[name]); err != nil {
		http.Error(w, "模板错误: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "渲染错误: "+err.Error(), http.StatusInternalServerError)
	}
}

func renderLogin(w http.ResponseWriter) {
	renderTemplate(w, "login", nil)
}

func renderMsg(w http.ResponseWriter, title, msg string) {
	renderTemplate(w, "msg", struct{ Title, Msg string }{title, msg})
}
