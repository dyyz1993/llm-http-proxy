package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- runChecks 基础测试 ----------------------------------------------------

func TestRunChecks_AllPass(t *testing.T) {
	// 所有拦截器都放行 → runChecks 返回 true
	checks := []CheckFunc{
		func(ctx *CheckContext) *CheckResult { return &CheckResult{} },
		func(ctx *CheckContext) *CheckResult { return &CheckResult{} },
	}
	w := httptest.NewRecorder()
	ctx := &CheckContext{}
	if !runChecks(w, checks, ctx) {
		t.Error("全部放行应返回 true")
	}
	if w.Code != 200 {
		t.Errorf("未拦截时应保持 200, got %d", w.Code)
	}
}

func TestRunChecks_FirstBlockStops(t *testing.T) {
	// 第一个拦截器阻止 → 不执行后续
	var secondRan bool
	checks := []CheckFunc{
		func(ctx *CheckContext) *CheckResult {
			return &CheckResult{Blocked: true, Status: 403, Message: "blocked"}
		},
		func(ctx *CheckContext) *CheckResult {
			secondRan = true
			return &CheckResult{}
		},
	}
	w := httptest.NewRecorder()
	ctx := &CheckContext{}
	if runChecks(w, checks, ctx) {
		t.Error("有拦截应返回 false")
	}
	if w.Code != 403 {
		t.Errorf("应返回 403, got %d", w.Code)
	}
	if secondRan {
		t.Error("第一个拦截器阻止后不应执行后续")
	}
}

func TestRunChecks_WithHeaders(t *testing.T) {
	// 验证拦截器设置的响应头被正确写入
	h := http.Header{}
	h.Set("Retry-After", "60")
	checks := []CheckFunc{
		func(ctx *CheckContext) *CheckResult {
			return &CheckResult{Blocked: true, Status: 429, Message: "too fast", Headers: h}
		},
	}
	w := httptest.NewRecorder()
	ctx := &CheckContext{}
	runChecks(w, checks, ctx)
	if w.Header().Get("Retry-After") != "60" {
		t.Errorf("Retry-After header 应被写入, got %q", w.Header().Get("Retry-After"))
	}
	if w.Code != 429 {
		t.Errorf("应返回 429, got %d", w.Code)
	}
}

// --- checkTimeBlock --------------------------------------------------------

func TestCheckTimeBlock_NotBlocked(t *testing.T) {
	ctx := &CheckContext{Config: KeyConfig{}}
	result := checkTimeBlock(ctx)
	if result.Blocked {
		t.Error("无 TimeBlock 配置不应阻止")
	}
}

func TestCheckTimeBlock_Blocked(t *testing.T) {
	ctx := &CheckContext{
		Config: KeyConfig{
			TimeBlock: &TimeBlock{Start: "00:00", End: "23:59"},
		},
	}
	result := checkTimeBlock(ctx)
	if !result.Blocked {
		t.Error("全天禁止时应阻止")
	}
	if result.Status != http.StatusForbidden {
		t.Errorf("应返回 403, got %d", result.Status)
	}
}

// --- checkQuota -----------------------------------------------------------

func TestCheckQuota_NoUsageTracker(t *testing.T) {
	ctx := &CheckContext{Usage: nil}
	result := checkQuota(ctx)
	if result.Blocked {
		t.Error("usageTracker=nil 不应阻止")
	}
}

func TestCheckQuota_NoQuotaConfig(t *testing.T) {
	us := newUsageStats()
	ctx := &CheckContext{Usage: us, Config: KeyConfig{}} // HasQuota=false
	result := checkQuota(ctx)
	if result.Blocked {
		t.Error("无配额配置不应阻止")
	}
}

func TestCheckQuota_UnderLimit(t *testing.T) {
	us := newUsageStats()
	ctx := &CheckContext{
		Alias:  "test",
		Usage:  us,
		Config: KeyConfig{MaxReqs: 10, Window: "1h"},
	}
	result := checkQuota(ctx)
	if result.Blocked {
		t.Error("未超限不应阻止")
	}
}

func TestCheckQuota_Exceeded(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 1, Window: "1h"}
	us.recordSuccess("test") // 已达上限

	ctx := &CheckContext{Alias: "test", Usage: us, Config: cfg}
	result := checkQuota(ctx)
	if !result.Blocked {
		t.Fatal("超限应阻止")
	}
	if result.Status != http.StatusPaymentRequired {
		t.Errorf("应返回 402, got %d", result.Status)
	}
	if result.Headers.Get("Retry-After") == "" {
		t.Error("超限应包含 Retry-After header")
	}
}

// --- checkRateLimit -------------------------------------------------------

func TestCheckRateLimit_Allowed(t *testing.T) {
	ks := newKeyStore()
	ks.configs["test"] = KeyConfig{Key: "sk-1", Rate: 60} // 足够快的速率
	ks.mu.Lock()
	ks.getOrCreateLimiter("test", ks.configs["test"])
	ks.mu.Unlock()

	ctx := &CheckContext{Alias: "test", Store: ks}
	result := checkRateLimit(ctx)
	if result.Blocked {
		t.Error("未超限流不应阻止")
	}
}

func TestCheckRateLimit_Blocked(t *testing.T) {
	ks := newKeyStore()
	ks.configs["test"] = KeyConfig{Key: "sk-1", Rate: 120, Burst: 1}
	ks.mu.Lock()
	ks.getOrCreateLimiter("test", ks.configs["test"])
	ks.mu.Unlock()

	// 消耗 burst
	ks.allow("test")

	ctx := &CheckContext{Alias: "test", Store: ks}
	result := checkRateLimit(ctx)
	if !result.Blocked {
		t.Fatal("超限流应阻止")
	}
	if result.Status != http.StatusTooManyRequests {
		t.Errorf("应返回 429, got %d", result.Status)
	}
	if result.Headers.Get("Retry-After") != "60" {
		t.Errorf("Retry-After 应为 60, got %q", result.Headers.Get("Retry-After"))
	}
}

// --- checkDomainWhitelist -------------------------------------------------

func TestCheckDomainWhitelist_Allowed(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("example.com")

	ctx := &CheckContext{
		Domain:   "example.com",
		Settings: sm,
	}
	result := checkDomainWhitelist(ctx)
	if result.Blocked {
		t.Error("在白名单不应阻止")
	}
}

func TestCheckDomainWhitelist_EmptyWhitelist(t *testing.T) {
	sm := newSettingsManager()

	ctx := &CheckContext{
		Domain:   "anything.com",
		Settings: sm,
	}
	result := checkDomainWhitelist(ctx)
	if result.Blocked {
		t.Error("空白名单 = 全部放行")
	}
}

func TestCheckDomainWhitelist_Blocked(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("allowed.com")

	ctx := &CheckContext{
		Domain:   "evil.com",
		Settings: sm,
	}
	result := checkDomainWhitelist(ctx)
	if !result.Blocked {
		t.Fatal("不在白名单应阻止")
	}
	if result.Status != http.StatusForbidden {
		t.Errorf("应返回 403, got %d", result.Status)
	}
}

// --- checkSetup -----------------------------------------------------------

func TestCheckSetup(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("test", KeyConfig{Key: "sk-1", Header: "Authorization"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer client-key")

	ctx := &CheckContext{
		Alias:   "test",
		Config:  ks.configs["test"],
		Request: req,
		Store:   ks,
	}
	result := checkSetup(ctx)
	if result.Blocked {
		t.Error("checkSetup 不应阻止")
	}
	if ctx.StatLabel != "key:test" {
		t.Errorf("StatLabel = %q, want key:test", ctx.StatLabel)
	}
	if ctx.HeadersToInject == nil {
		t.Error("HeadersToInject 不应为 nil")
	}
}

// --- checkSetupPassthrough ------------------------------------------------

func TestCheckSetupPassthrough_WithStore(t *testing.T) {
	ks := newKeyStore()
	ctx := &CheckContext{Store: ks}
	result := checkSetupPassthrough(ctx)
	if result.Blocked {
		t.Error("checkSetupPassthrough 不应阻止")
	}
}

func TestCheckSetupPassthrough_WithoutStore(t *testing.T) {
	ctx := &CheckContext{Store: nil}
	result := checkSetupPassthrough(ctx)
	if result.Blocked {
		t.Error("Store=nil 时 checkSetupPassthrough 不应阻止")
	}
}

// --- 端到端测试:完整拦截器链通过 HTTP --------------------------------------

func TestInterceptorChain_E2E_Pass(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.setConfig("test", KeyConfig{
		Key:    "sk-test",
		Header: "Authorization",
	})
	oldSettings := settingsMgr
	settingsMgr = newSettingsManager()
	defer func() { settingsMgr = oldSettings }()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/k/") {
			handleKeyRoute(w, req, ks, nil, nil)
			return
		}
		http.Error(w, "not proxy", http.StatusBadRequest)
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/k/test/" + backend.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("所有检查通过应返回 200, got %d", resp.StatusCode)
	}
}

func TestInterceptorChain_E2E_QuotaBlocked(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	cfg := KeyConfig{Key: "sk-test", Header: "Authorization", MaxReqs: 1, Window: "1h"}
	ks.setConfig("test", cfg)
	oldSettings := settingsMgr
	settingsMgr = newSettingsManager()
	defer func() { settingsMgr = oldSettings }()

	oldTracker := usageTracker
	usageTracker = newUsageStats()
	defer func() { usageTracker = oldTracker }()
	usageTracker.recordSuccess("test") // 已达上限

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/k/") {
			handleKeyRoute(w, req, ks, nil, usageTracker)
			return
		}
		http.Error(w, "not proxy", http.StatusBadRequest)
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/k/test/" + backend.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 402 {
		t.Errorf("配额超限应返回 402, got %d", resp.StatusCode)
	}
}
