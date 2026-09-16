package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReorderPinnedFirst_Basic 置顶成员提到最前,其余保持相对顺序;
// 多个置顶成员之间按原列表顺序排列。
func TestReorderPinnedFirst_Basic(t *testing.T) {
	got := reorderPinnedFirst([]string{"c"}, []string{"a", "b", "c", "d"})
	if len(got) != 4 || got[0] != "c" || got[1] != "a" || got[2] != "b" || got[3] != "d" {
		t.Errorf("置顶 c 应排最前,其余保序, got %v", got)
	}
}

// TestReorderPinnedFirst_MultiplePinned 多个置顶成员按原列表相对顺序排前面。
func TestReorderPinnedFirst_MultiplePinned(t *testing.T) {
	got := reorderPinnedFirst([]string{"d", "b"}, []string{"a", "b", "c", "d"})
	// pinned 里写的是 [d, b],但成员列表里 b 在 d 前面 → 前缀应为 [b, d]
	if len(got) != 4 || got[0] != "b" || got[1] != "d" || got[2] != "a" || got[3] != "c" {
		t.Errorf("多个置顶按原列表顺序, got %v", got)
	}
}

// TestReorderPinnedFirst_EdgeCases 空 pinned 原样返回;pinned 含非成员忽略;members 空。
func TestReorderPinnedFirst_EdgeCases(t *testing.T) {
	members := []string{"a", "b", "c"}
	got := reorderPinnedFirst(nil, members)
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("空 pinned 应原样返回, got %v", got)
	}
	got = reorderPinnedFirst([]string{"x", "a"}, members)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("pinned 中的非成员应忽略, got %v", got)
	}
	if got := reorderPinnedFirst([]string{"a"}, nil); got != nil {
		t.Errorf("members 为 nil 应返回 nil, got %v", got)
	}
}

// TestReorderPinnedFirst_OverridesDynamicTiers 置顶压过动态排序所有档位:
// 周免成员(unit=6)本来被垫底(deferred),置顶后应排最前。
func TestReorderPinnedFirst_OverridesDynamicTiers(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "normal", Level: "pro", Limits: []quotaLimit{lim(3, 10, futureReset(300))}},
		{Alias: "weekly", Level: "pro", Limits: []quotaLimit{
			lim(3, 5, futureReset(300)),
			lim(6, 1, futureReset(7000)), // 周额度 → deferred 垫底
		}},
	})
	sorted := sortGroupMembersDynamic([]string{"weekly", "normal"}, qc, 0)
	if sorted[0] != "normal" {
		t.Fatalf("前置条件失败:动态排序后 normal 应在前, got %v", sorted)
	}
	got := reorderPinnedFirst([]string{"weekly"}, sorted)
	if len(got) != 2 || got[0] != "weekly" {
		t.Errorf("置顶的周免成员应压过垫底档排最前, got %v", got)
	}
}

// TestFilterPinnedMembers 过滤置顶列表只保留现有成员。
func TestFilterPinnedMembers(t *testing.T) {
	got := filterPinnedMembers([]string{"a", "x", "b"}, []string{"a", "b", "c"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("应剔除已移除的成员 x, got %v", got)
	}
	if got := filterPinnedMembers(nil, []string{"a"}); got != nil {
		t.Errorf("空 pinned 应返回 nil, got %v", got)
	}
	if got := filterPinnedMembers([]string{"x"}, []string{"a", "b"}); got != nil {
		t.Errorf("全部无效时应返回 nil, got %v", got)
	}
}

// TestRemoveFromSlice 返回不含目标元素的新切片。
func TestRemoveFromSlice(t *testing.T) {
	orig := []string{"a", "b", "c"}
	got := removeFromSlice(orig, "b")
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("应移除 b, got %v", got)
	}
	if len(orig) != 3 || orig[1] != "b" {
		t.Errorf("原切片不应被修改, got %v", orig)
	}
	if got := removeFromSlice([]string{"a"}, "a"); got != nil {
		t.Errorf("移除后为空应返回 nil, got %v", got)
	}
}

// TestGroupConfigPinnedYAML pinned 字段的 YAML 序列化/反序列化往返。
func TestGroupConfigPinnedYAML(t *testing.T) {
	in := "groups:\n  pool:\n    members: [a, b]\n    pinned: [b]\n    cooldown: 5m\n"
	var parsed struct {
		Groups map[string]GroupConfig `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(in), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	cfg, ok := parsed.Groups["pool"]
	if !ok {
		t.Fatal("缺少 group pool")
	}
	if len(cfg.Pinned) != 1 || cfg.Pinned[0] != "b" {
		t.Errorf("pinned 解析错误, got %v", cfg.Pinned)
	}

	out, err := yaml.Marshal(map[string]GroupConfig{"pool": cfg})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var rt map[string]GroupConfig
	if err := yaml.Unmarshal(out, &rt); err != nil {
		t.Fatalf("往返解析失败: %v", err)
	}
	if len(rt["pool"].Pinned) != 1 || rt["pool"].Pinned[0] != "b" {
		t.Errorf("pinned 往返丢失, got %v", rt["pool"].Pinned)
	}
}

// TestGroupRoute_E2E_PinnedMemberWins 端到端验证置顶:
//  1. members 顺序 [m1, m2] 且置顶 m2 → 请求应全部打到 m2(置顶压过列表顺序和动态排序)
//  2. m2 返回 502 命中 on_status 进入冷却 → 请求自动回落到 m1
func TestGroupRoute_E2E_PinnedMemberWins(t *testing.T) {
	// 后端根据 Authorization 区分成员;m2Fail 置 1 时 sk-m2 返回 502
	var m2Fail int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "sk-m2") && atomic.LoadInt32(&m2Fail) == 1:
			w.WriteHeader(502)
			fmt.Fprintln(w, "bad gateway from m2")
		case strings.Contains(auth, "sk-m2"):
			fmt.Fprintln(w, "from-m2")
		default:
			fmt.Fprintln(w, "from-m1")
		}
	}))
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-m1", Header: "Authorization", Prefix: "Bearer "}
	ks.configs["m2"] = KeyConfig{Key: "sk-m2", Header: "Authorization", Prefix: "Bearer "}
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1", "m2"}, Pinned: []string{"m2"}, OnStatus: []int{502}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	if settingsMgr == nil {
		settingsMgr = newSettingsManager()
	}
	// 动态排序依赖全局 quotaCache,测试环境强制 nil(成员按配置顺序,验证置顶单独生效)
	oldQC := quotaCacheInst
	quotaCacheInst = nil
	defer func() { quotaCacheInst = oldQC }()

	stats := newStatsCollector()
	us := newUsageStats()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/g/") {
			handleGroupRoutePrefix(w, req, ks, stats, us)
			return
		}
		http.Error(w, "no route", 500)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	get := func() string {
		t.Helper()
		resp, err := http.Get(proxy.URL + "/g/pool/" + backend.URL + "/echo")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b))
	}

	// 阶段1:m2 在列表里排第二,置顶后所有请求应打到 m2
	for i := 0; i < 3; i++ {
		if got := get(); got != "from-m2" {
			t.Fatalf("置顶成员 m2 应被优先使用(第 %d 次请求), got %q", i+1, got)
		}
	}

	// 阶段2:m2 挂了(502 命中 on_status)→ 冷却换人,回落到列表第一位的 m1
	atomic.StoreInt32(&m2Fail, 1)
	if got := get(); got != "from-m1" {
		t.Fatalf("置顶成员冷却期间应回落到 m1, got %q", got)
	}
}
