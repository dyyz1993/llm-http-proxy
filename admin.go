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
	"net/http"
	"strings"
	"time"
)

// cookieName 是登录态 cookie 的名字。
const cookieName = "lhp_admin"
const cookieTTL = 24 * time.Hour

// adminServer 是管理界面的核心,持有所有依赖。
type adminServer struct {
	password string // 登录密码
	secret   []byte // HMAC 签名密钥(进程启动时随机生成)
	stats    *statsCollector
	keys     *keyStore // 可能为 nil(未启用 -keys 时)
}

// newAdminServer 创建管理界面。password 为空则不启用(返回 nil)。
func newAdminServer(password string, stats *statsCollector, ks *keyStore) *adminServer {
	if password == "" {
		return nil
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	return &adminServer{
		password: password,
		secret:   secret,
		stats:    stats,
		keys:     ks,
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
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    a.makeCookieValue(),
				Path:     "/",
				HttpOnly: true,
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
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
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
	}{
		Version:   version,
		BuildTime: buildTime,
		StartTime: startTime.In(beijing).Format("2006-01-02 15:04:05"),
		Uptime:    time.Since(startTime).Round(time.Second).String(),
		TotalIPs:  len(snap),
		TotalReq:  totalCount(snap),
		KeysCount: len(a.keys.allConfigs()),
	}
	renderTemplate(w, "dashboard", data)
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
	renderTemplate(w, "keys", a.keys.allConfigs())
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
	prefix := strings.TrimSpace(r.FormValue("prefix"))
	rate := 0
	burst := 0
	fmt.Sscanf(r.FormValue("rate"), "%d", &rate)
	fmt.Sscanf(r.FormValue("burst"), "%d", &burst)

	if alias == "" || key == "" {
		renderMsg(w, "错误", "alias 和 key 不能为空")
		return
	}
	if header == "" {
		header = "Authorization"
	}
	cfg := KeyConfig{Key: key, Header: header, Prefix: prefix, Rate: rate, Burst: burst}
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

// --- 模板渲染 ---

func renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	tmpl := template.New(name).Funcs(template.FuncMap{
		"mul100": func(v float64) float64 { return v * 100 },
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
