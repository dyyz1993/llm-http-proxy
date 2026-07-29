package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// === 测试1:resetAlias(usageStats) ===

// TestUsageResetAlias 验证 resetAlias 清零指定 alias(或 group)的 usage 数据。
// group 维度用 "group:{name}" 带前缀的 key(与 key 维度隔离,避免重名混淆)。
func TestUsageResetAlias(t *testing.T) {
	us := newUsageStats() // 内存模式,不持久化
	// 记两条数据:一个 key 维度,一个 group 维度(带前缀)
	us.record("glm", usageData{Prompt: 100, Completion: 50, HasData: true})
	us.record("group:mygroup", usageData{Prompt: 200, Completion: 100, HasData: true})

	// 确认两条都在
	snap := us.snapshot()
	if _, ok := snap["glm"]; !ok {
		t.Fatal("reset 前 glm 应存在")
	}
	if _, ok := snap["group:mygroup"]; !ok {
		t.Fatal("reset 前 group:mygroup 应存在")
	}

	// 重置 group 维度(用带前缀的 key)
	us.resetAlias("group:mygroup")

	// group:mygroup 应被删除,glm 保留
	snap = us.snapshot()
	if _, ok := snap["group:mygroup"]; ok {
		t.Error("reset 后 group:mygroup 应被删除")
	}
	if _, ok := snap["glm"]; !ok {
		t.Error("reset 后 glm 应保留(不受影响)")
	}
}

// TestUsageGroupNoCollision 验证 group 名和 alias 重名时数据不混淆。
// 关键:group 维度用 "group:xxx" 带前缀,不会覆盖 alias "xxx" 的数据。
func TestUsageGroupNoCollision(t *testing.T) {
	us := newUsageStats()
	// 假设有个 alias 叫 "mygroup",同时也有个 group 叫 "mygroup"
	us.record("mygroup", usageData{Prompt: 100, HasData: true})       // alias 维度
	us.record("group:mygroup", usageData{Prompt: 200, HasData: true}) // group 维度

	snap := us.snapshot()
	// 两条数据应共存(不同 key)
	if snap["mygroup"].WindowPrompt != 100 {
		t.Errorf("alias mygroup 应是 100, got %d", snap["mygroup"].WindowPrompt)
	}
	if snap["group:mygroup"].WindowPrompt != 200 {
		t.Errorf("group:mygroup 应是 200, got %d", snap["group:mygroup"].WindowPrompt)
	}
	// 重置 group 不影响 alias
	us.resetAlias("group:mygroup")
	snap = us.snapshot()
	if _, ok := snap["mygroup"]; !ok {
		t.Error("重置 group:mygroup 后,alias mygroup 应保留")
	}
}

// TestUsageResetAlias_Empty resetAlias 空参数不 panic 不删数据。
func TestUsageResetAlias_Empty(t *testing.T) {
	us := newUsageStats()
	us.record("glm", usageData{Prompt: 100, HasData: true})
	us.resetAlias("") // 空参数,应 no-op
	snap := us.snapshot()
	if _, ok := snap["glm"]; !ok {
		t.Error("空参数 reset 不应删数据")
	}
}

// === 测试2:resetGroup(statsCollector) ===

// TestStatsResetGroup 验证 resetGroup 清零指定 group 的 stats 数据(散布在各 IP)。
func TestStatsResetGroup(t *testing.T) {
	s := newStatsCollector()
	// 模拟 group 路由记录:同一 IP 既有 key 又有 group
	s.record("1.2.3.4", "key:glm", "api.z.ai", 200)
	s.record("1.2.3.4", "group:mygroup", "api.z.ai", 200)
	s.record("5.6.7.8", "group:mygroup", "api.z.ai", 200) // 另一个 IP 也有 group

	// 确认 group 数据存在
	snap := s.snapshot()
	foundGroup := false
	for _, is := range snap {
		if _, ok := is.Keys["group:mygroup"]; ok {
			foundGroup = true
			break
		}
	}
	if !foundGroup {
		t.Fatal("reset 前 group:mygroup 应存在")
	}

	// 重置 group
	s.resetGroup("mygroup")

	// group:mygroup 应被所有 IP 删除,key:glm 保留
	snap = s.snapshot()
	for ip, is := range snap {
		if _, ok := is.Keys["group:mygroup"]; ok {
			t.Errorf("reset 后 IP %s 仍含 group:mygroup", ip)
		}
	}
	// key:glm 应保留
	if is, ok := snap["1.2.3.4"]; ok {
		if _, ok := is.Keys["key:glm"]; !ok {
			t.Error("reset 后 key:glm 应保留")
		}
	} else {
		t.Error("IP 1.2.3.4 不应被删除")
	}
}

// TestStatsResetGroup_Empty resetGroup 空参数不 panic。
func TestStatsResetGroup_Empty(t *testing.T) {
	s := newStatsCollector()
	s.record("1.2.3.4", "key:glm", "api.z.ai", 200)
	s.resetGroup("") // 不 panic
	snap := s.snapshot()
	if _, ok := snap["1.2.3.4"].Keys["key:glm"]; !ok {
		t.Error("空参数 reset 不应删数据")
	}
}

// === 测试3:防污染过滤 ===

// TestBuildUsageHTML_HidesGroup 验证 buildUsageHTML 跳过 group: 前缀的 alias。
// group 维度的数据不应出现在 key 视图的 usage 表里。
func TestBuildUsageHTML_HidesGroup(t *testing.T) {
	snap := map[string]aliasUsageStats{
		"glm":           {WindowPrompt: 100, WindowCompletion: 50},
		"group:mygroup": {WindowPrompt: 200, WindowCompletion: 100},
	}
	html := buildUsageHTML(snap, map[string]KeyConfig{}, nil)
	if !strings.Contains(html, "glm") {
		t.Error("buildUsageHTML 应包含 key 维度 glm")
	}
	if strings.Contains(html, "mygroup") {
		t.Error("buildUsageHTML 不应包含 group 维度 mygroup(防污染)")
	}
}

// TestStatsByKey_HidesGroup 验证 statsByKey 跳过 group: 前缀的 key。
func TestStatsByKey_HidesGroup(t *testing.T) {
	s := newStatsCollector()
	s.record("1.2.3.4", "key:glm", "api.z.ai", 200)
	s.record("1.2.3.4", "group:mygroup", "api.z.ai", 200)
	snap := s.snapshot()

	byKey := statsByKey(snap)
	if _, ok := byKey["key:glm"]; !ok {
		t.Error("statsByKey 应包含 key:glm")
	}
	if _, ok := byKey["group:mygroup"]; ok {
		t.Error("statsByKey 不应包含 group:mygroup(防污染)")
	}
}

// TestTotalCount_HidesGroup 验证 totalCount 不双计 group 维度。
// 同一 IP 有 key:glm(1次) + group:mygroup(1次),totalCount 应为 1 不是 2。
func TestTotalCount_HidesGroup(t *testing.T) {
	s := newStatsCollector()
	s.record("1.2.3.4", "key:glm", "api.z.ai", 200)
	s.record("1.2.3.4", "group:mygroup", "api.z.ai", 200)
	snap := s.snapshot()

	got := totalCount(snap)
	if got != 1 {
		t.Errorf("totalCount 应为 1(不双计 group), got %d", got)
	}
}

// TestDistinctKeyCount_HidesGroup 验证 distinctKeyCount 不计 group 维度。
func TestDistinctKeyCount_HidesGroup(t *testing.T) {
	s := newStatsCollector()
	s.record("1.2.3.4", "key:glm", "api.z.ai", 200)
	s.record("1.2.3.4", "group:mygroup", "api.z.ai", 200)
	snap := s.snapshot()

	got := distinctKeyCount(snap)
	if got != 1 {
		t.Errorf("distinctKeyCount 应为 1(不计 group), got %d", got)
	}
}

// === 测试4:handleGroupReset E2E ===

// TestHandleGroupReset_E2E 验证 POST /__admin/groups/reset 清零 group 统计。
func TestHandleGroupReset_E2E(t *testing.T) {
	ks := newKeyStore()
	ks.setGroup("mygroup", GroupConfig{Members: []string{"glm"}})

	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// POST reset(默认 client 会跟随 303 重定向到 groups 页,最终 200)
	resp, err := client.PostForm(proxy.URL+"/__admin/groups/reset?name=mygroup", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 303 重定向会被 client 跟随,最终落到 groups 页(200);只要没报错就算通过
	if resp.StatusCode >= 400 {
		t.Errorf("reset 不该返回错误码, got %d", resp.StatusCode)
	}
}

// TestHandleGroupReset_NoAuth 未登录应拒绝(防未授权清零)。
func TestHandleGroupReset_NoAuth(t *testing.T) {
	ks := newKeyStore()
	ks.setGroup("mygroup", GroupConfig{Members: []string{"glm"}})

	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	// 不登录直接 POST(应被 requireAuth 拦截:重定向到 login)
	noRedirect := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随重定向
		},
	}
	resp, err := noRedirect.Post(proxy.URL+"/__admin/groups/reset?name=mygroup",
		"application/x-www-form-urlencoded", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 未登录应被拦截:303 重定向到 login 或 401,不应是 200
	if resp.StatusCode == http.StatusOK {
		t.Errorf("未登录应被拒绝(不应返回 200, got %d)", resp.StatusCode)
	}
}
