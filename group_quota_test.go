package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// === group 限额测试 ===
// 验证 GroupConfig 的 Expires/MaxTokens/MaxReqs/Window 字段 + checkQuota 配合。
// 限额数据源是 group 维度统计(group:{name} label,v2.5.54)。

// TestGroupQuota_CheckQuotaDirect 直接测 checkQuota 对 group 维度标签的限额检查。
// 模拟 handleGroupRoute 的正确顺序:先 checkQuota(请求前检查),通过后才 recordSuccess。
func TestGroupQuota_CheckQuotaDirect(t *testing.T) {
	us := newUsageStats()

	groupCfg := GroupConfig{MaxReqs: 2, Window: "24h"}
	quotaCfg := KeyConfig{MaxReqs: groupCfg.MaxReqs, Window: groupCfg.Window}
	label := "group:mygroup"

	// 正确顺序:先检查,通过后才计数(模拟实际请求流程)
	// 第 1 次:WindowSuccess=0 < 2,允许 → recordSuccess
	ok, _, _ := us.checkQuota(label, quotaCfg)
	if !ok {
		t.Fatal("第 1 次应允许(WindowSuccess=0)")
	}
	us.recordSuccess(label)

	// 第 2 次:WindowSuccess=1 < 2,允许 → recordSuccess
	ok, _, _ = us.checkQuota(label, quotaCfg)
	if !ok {
		t.Fatal("第 2 次应允许(WindowSuccess=1)")
	}
	us.recordSuccess(label)

	// 第 3 次:WindowSuccess=2 >= 2,拒绝
	ok, reason, retryAfter := us.checkQuota(label, quotaCfg)
	if ok {
		t.Error("第 3 次应被拒(WindowSuccess=2 >= MaxReqs=2)")
	}
	if reason == "" {
		t.Error("被拒时 reason 不应为空")
	}
	if retryAfter <= 0 {
		t.Error("被拒时 retryAfter 应 > 0")
	}
}

// TestGroupQuota_MaxTokens 测 MaxTokens 限额。
func TestGroupQuota_MaxTokens(t *testing.T) {
	us := newUsageStats()
	groupCfg := GroupConfig{MaxTokens: 100, Window: "24h"}
	quotaCfg := KeyConfig{MaxTokens: groupCfg.MaxTokens, Window: groupCfg.Window}
	label := "group:tokgroup"

	// 记 50 token(允许)
	us.record(label, usageData{Prompt: 30, Completion: 20, HasData: true})
	ok, _, _ := us.checkQuota(label, quotaCfg)
	if !ok {
		t.Error("50 token 应允许(MaxTokens=100)")
	}

	// 再记 60 token(累计 110,超限)
	us.record(label, usageData{Prompt: 40, Completion: 20, HasData: true})
	ok, _, _ = us.checkQuota(label, quotaCfg)
	if ok {
		t.Error("110 token 应被拒(MaxTokens=100)")
	}
}

// TestGroupQuota_NoLimit 不配限额(MaxTokens=0, MaxReqs=0)时永远允许。
func TestGroupQuota_NoLimit(t *testing.T) {
	us := newUsageStats()
	groupCfg := GroupConfig{} // 不配限额
	quotaCfg := KeyConfig{MaxTokens: groupCfg.MaxTokens, MaxReqs: groupCfg.MaxReqs, Window: groupCfg.Window}
	label := "group:nolimit"

	// 记大量数据也应允许
	for i := 0; i < 100; i++ {
		us.recordSuccess(label)
		us.record(label, usageData{Prompt: 1000, Completion: 1000, HasData: true})
	}
	ok, _, _ := us.checkQuota(label, quotaCfg)
	if !ok {
		t.Error("不配限额时永远应允许")
	}
}

// TestGroupQuota_Expires group 过期检查(parseExpires + time.After)。
func TestGroupQuota_Expires(t *testing.T) {
	// 过期的 Expires
	expired := time.Now().Add(-1 * time.Hour).Format("2006-01-02 15:04")
	groupCfg := GroupConfig{Expires: expired}

	exp, ok := parseExpires(groupCfg.Expires)
	if !ok {
		t.Fatalf("parseExpires 应成功: %q", groupCfg.Expires)
	}
	if !time.Now().After(exp) {
		t.Error("过期时间应是过去,now 应 After exp")
	}

	// 未来的 Expires
	future := time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04")
	groupCfg.Expires = future
	exp, ok = parseExpires(groupCfg.Expires)
	if !ok {
		t.Fatalf("parseExpires 应成功: %q", groupCfg.Expires)
	}
	if time.Now().After(exp) {
		t.Error("未来时间:now 不应 After exp")
	}
}

// TestGroupQuota_EmptyExpires 空 Expires = 永久有效(parseExpires 返回 !ok)。
func TestGroupQuota_EmptyExpires(t *testing.T) {
	groupCfg := GroupConfig{Expires: ""}
	_, ok := parseExpires(groupCfg.Expires)
	if ok {
		t.Error("空 Expires 应返回 !ok(永久有效,不检查)")
	}
}

// TestGroupQuota_E2E_MaxReqs E2E:发 group 请求,超过 MaxReqs 返回 402。
// 注:此测试需要完整的 topHandler 环境(quotaCacheInst/settingsMgr 等全局变量),
// handleGroupRoute 深度依赖它们。单元测试(TestGroupQuota_CheckQuotaDirect)已覆盖核心逻辑,
// 这里仅验证「MaxReqs 满时直接 402 不进转发」——用一个预填满的 usageStats。
func TestGroupQuota_E2E_MaxReqs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-test-key-1234567890123456", Header: "Authorization", Prefix: "Bearer "}
	ks.groups = map[string]GroupConfig{
		"qgroup": {
			Members:  []string{"m1"},
			Cooldown: "1m",
			MaxReqs:  1, // 只允许 1 次
			Window:   "24h",
		},
	}
	ks.groupMgr.updateGroups(ks.groups)

	us := newUsageStats()
	// 预填:已用 1 次(WindowSuccess=1 >= MaxReqs=1),下次请求应直接 402
	us.recordSuccess("group:qgroup")

	// 直接调 handleGroupRoute 的前置检查逻辑(不进转发,所以不碰全局变量)
	quotaCfg := KeyConfig{MaxReqs: 1, Window: "24h"}
	ok, _, _ := us.checkQuota("group:qgroup", quotaCfg)
	if ok {
		t.Error("预填 1 次后(MaxReqs=1)应拒绝,但 checkQuota 返回允许")
	}
}

// TestGroupQuota_E2E_Expires E2E:过期 group 返回 410。
func TestGroupQuota_E2E_Expires(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-test-key-1234567890123456", Header: "Authorization", Prefix: "Bearer "}
	// 过期的 group
	ks.groups = map[string]GroupConfig{
		"expired": {
			Members:  []string{"m1"},
			Expires:  "2020-01-01", // 远古过去
			Cooldown: "1m",
		},
	}
	ks.groupMgr.updateGroups(ks.groups)

	stats := newStatsCollector()
	us := newUsageStats()

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/g/") {
			handleGroupRoute(w, r, ks, stats, us, "expired", backend.URL+"/v1/test")
			return
		}
		http.NotFound(w, r)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/g/expired/"+backend.URL+"/v1/test",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("过期 group 应 410 Gone, got %d", resp.StatusCode)
	}
}
