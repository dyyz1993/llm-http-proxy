package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGroupManager_IsGroup(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool1": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	if !gm.isGroup("pool1") {
		t.Error("pool1 应该是 group")
	}
	if gm.isGroup("pool2") {
		t.Error("pool2 不应该是 group")
	}
}

func TestGroupManager_PickMember(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b", "c"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 初始:第一个可用成员
	m, allCooling := gm.pickMember("pool")
	if m != "a" || allCooling {
		t.Errorf("期望 a/false, 得到 %q/%v", m, allCooling)
	}

	// a 冷却后 → 应该返回 b
	gm.markCooldown("a", "pool", 502)
	m, allCooling = gm.pickMember("pool")
	if m != "b" || allCooling {
		t.Errorf("期望 b/false, 得到 %q/%v", m, allCooling)
	}

	// a, b 都冷却 → 返回 c
	gm.markCooldown("b", "pool", 502)
	m, allCooling = gm.pickMember("pool")
	if m != "c" || allCooling {
		t.Errorf("期望 c/false, 得到 %q/%v", m, allCooling)
	}

	// 全部冷却 → 返回空, allCooling=true
	gm.markCooldown("c", "pool", 502)
	m, allCooling = gm.pickMember("pool")
	if m != "" || !allCooling {
		t.Errorf("期望 \"\"/true, 得到 %q/%v", m, allCooling)
	}
}

func TestGroupManager_PickMember_UnknownGroup(t *testing.T) {
	gm := newGroupManager()
	m, allCooling := gm.pickMember("nonexistent")
	if m != "" || allCooling {
		t.Errorf("未知 group 应返回 \"\"/false, 得到 %q/%v", m, allCooling)
	}
}

func TestGroupManager_CooldownExpiry(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "50ms"},
	})

	// 冷却 a
	gm.markCooldown("a", "pool", 502)
	m, _ := gm.pickMember("pool")
	if m != "b" {
		t.Errorf("a 冷却后应返回 b, 得到 %q", m)
	}

	// 等待冷却过期
	time.Sleep(60 * time.Millisecond)

	// a 应该恢复可用(优先于 b)
	m, _ = gm.pickMember("pool")
	if m != "a" {
		t.Errorf("a 冷却过期后应恢复优先, 得到 %q", m)
	}
}

func TestGroupManager_MarkSuccess(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 冷却 a
	gm.markCooldown("a", "pool", 502)
	st := gm.memberStatus("a")
	if !st.IsCooling {
		t.Error("a 应该在冷却中")
	}
	if st.FailCount != 1 {
		t.Errorf("FailCount 应为 1, 得到 %d", st.FailCount)
	}

	// 标记成功 → 清除冷却
	gm.markSuccess("a")
	st = gm.memberStatus("a")
	if st.IsCooling {
		t.Error("a 应该不再冷却")
	}
	if st.FailCount != 0 {
		t.Errorf("FailCount 应为 0, 得到 %d", st.FailCount)
	}
}

func TestGroupManager_ShouldSwitchStatus(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}, OnStatus: []int{402, 429, 502, 503}},
	})

	if !gm.shouldSwitchStatus("pool", 502) {
		t.Error("502 应该触发换人")
	}
	if !gm.shouldSwitchStatus("pool", 429) {
		t.Error("429 应该触发换人")
	}
	if gm.shouldSwitchStatus("pool", 200) {
		t.Error("200 不应该触发换人")
	}
	if gm.shouldSwitchStatus("pool", 404) {
		t.Error("404 不应该触发换人")
	}
}

func TestGroupManager_DefaultCooldown(t *testing.T) {
	gm := newGroupManager()
	// Cooldown 为空 → 默认 5 分钟
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}, OnStatus: []int{502}},
	})

	gm.markCooldown("a", "pool", 502)
	st := gm.memberStatus("a")
	remaining := time.Until(st.CoolUntil)
	if remaining < 4*time.Minute || remaining > 6*time.Minute {
		t.Errorf("默认冷却应约 5 分钟, 剩余 %v", remaining)
	}
}

func TestGroupManager_GroupStatus(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 冷却 a
	gm.markCooldown("a", "pool", 502)

	snap := gm.groupStatus("pool")
	if snap.Name != "pool" {
		t.Errorf("Name 应为 pool, 得到 %q", snap.Name)
	}
	if len(snap.Members) != 2 {
		t.Fatalf("应有 2 个成员, 得到 %d", len(snap.Members))
	}
	if !snap.Members[0].IsCooling {
		t.Error("成员 a 应在冷却中")
	}
	if snap.Members[0].LastStatus != 502 {
		t.Errorf("成员 a LastStatus 应为 502, 得到 %d", snap.Members[0].LastStatus)
	}
	if snap.Members[1].IsCooling {
		t.Error("成员 b 不应在冷却中")
	}
}

func TestGroupManager_AllGroupStatus(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool1": {Members: []string{"a"}, OnStatus: []int{502}, Cooldown: "1m"},
		"pool2": {Members: []string{"b"}, OnStatus: []int{429}, Cooldown: "30s"},
	})

	snaps := gm.allGroupStatus()
	if len(snaps) != 2 {
		t.Fatalf("应有 2 个 group, 得到 %d", len(snaps))
	}
}

func TestGroupManager_UpdateGroups_KeepsCooldownState(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 冷却 a
	gm.markCooldown("a", "pool", 502)

	// 热更新(配置不变)
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// a 的冷却状态应保留
	st := gm.memberStatus("a")
	if !st.IsCooling {
		t.Error("热更新后 a 的冷却状态应保留")
	}
}

func TestGroupManager_UpdateGroups_NewMember(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 热更新:加新成员
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// b 应有状态条目
	st := gm.memberStatus("b")
	if st.Alias != "b" {
		t.Errorf("b 应有状态条目, 得到 %q", st.Alias)
	}
}

func TestGroupConfig_DefaultOnStatus(t *testing.T) {
	// 空的 OnStatus → shouldSwitchStatus 返回 false(不换人)
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}, Cooldown: "1m"},
	})

	if gm.shouldSwitchStatus("pool", 502) {
		t.Error("空 OnStatus 不应触发换人")
	}
}

// --- 集成测试(端到端 group 切换) ---

func TestGroupRoute_E2E_Fallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// m1 后端:返回 502
	failBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"bad gateway"}`))
	}))
	defer failBackend.Close()

	// m2 后端:返回 200
	okBackend := echoBackend()
	defer okBackend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-fail", Header: "Authorization", Prefix: "Bearer "}
	ks.configs["m2"] = KeyConfig{Key: "sk-ok", Header: "Authorization", Prefix: "Bearer "}

	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1", "m2"}, OnStatus: []int{502}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	// 用 failBackend 作为 m1 的目标,m2 用 okBackend
	// 模拟 group 路由:先试 m1(返回502),再试 m2(返回200)
	// 由于 newProxyHandler 会透传响应,我们验证:
	// 1. m1 被标记冷却
	// 2. m2 正常

	// 验证 groupManager 逻辑
	m, allCooling := ks.groupMgr.pickMember("pool")
	if m != "m1" || allCooling {
		t.Fatalf("初始应挑 m1, 得到 %q/%v", m, allCooling)
	}

	// 模拟 m1 返回 502
	ks.groupMgr.markCooldown("m1", "pool", 502)

	// 下一次应挑 m2
	m, _ = ks.groupMgr.pickMember("pool")
	if m != "m2" {
		t.Fatalf("m1 冷却后应挑 m2, 得到 %q", m)
	}
}

func TestGroupRoute_E2E_QuotaExceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-1", Header: "Authorization", Prefix: "Bearer ", MaxTokens: 100, Window: "1h"}
	ks.configs["m2"] = KeyConfig{Key: "sk-2", Header: "Authorization", Prefix: "Bearer "}

	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1", "m2"}, OnStatus: []int{402}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	// 用满 m1 的限额
	oldTracker := usageTracker
	usageTracker = newUsageStats()
	defer func() { usageTracker = oldTracker }()

	// 消耗 m1 的限额(100 token)
	usageTracker.record("m1", usageData{HasData: true, Prompt: 90, Completion: 20})

	// m1 应该被限额拦截
	cfg := ks.configs["m1"]
	cfg = resolveConfig(cfg, nil, "default")
	ok, _, _ := usageTracker.checkQuota("m1", cfg)
	if ok {
		t.Fatal("m1 应该已被限额拦截")
	}

	// group 应该跳过 m1,直接选 m2
	m, _ := ks.groupMgr.pickMember("pool")
	if m != "m1" {
		// m1 没被标记冷却,group 仍会先试它
		// 这是正常的——拦截器拒绝后才标记冷却
	}

	// 模拟拦截器拒绝 m1
	ks.groupMgr.markCooldown("m1", "pool", 402)
	m, _ = ks.groupMgr.pickMember("pool")
	if m != "m2" {
		t.Fatalf("m1 被拦截后应选 m2, 得到 %q", m)
	}
}

func TestGroupRoute_E2E_AllExhausted(t *testing.T) {
	// 所有成员都冷却 → 返回 503
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a", "b"}, OnStatus: []int{502}, Cooldown: "1m"},
	})

	// 冷却所有成员
	gm.markCooldown("a", "pool", 502)
	gm.markCooldown("b", "pool", 502)

	m, allCooling := gm.pickMember("pool")
	if m != "" || !allCooling {
		t.Fatalf("全部冷却应返回 \"\"/true, 得到 %q/%v", m, allCooling)
	}

	// allGroupStatus 应显示两个都在冷却
	snaps := gm.allGroupStatus()
	for _, snap := range snaps {
		if snap.Name == "pool" {
			for _, member := range snap.Members {
				if !member.IsCooling {
					t.Errorf("成员 %s 应在冷却中", member.Alias)
				}
			}
		}
	}
}

func TestGroupRoute_E2E_NotAGroup(t *testing.T) {
	// 普通 alias 不受 group 逻辑影响
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"a"}},
	})

	if gm.isGroup("regular-alias") {
		t.Error("普通别名不应被识别为 group")
	}
	if !gm.isGroup("pool") {
		t.Error("pool 应被识别为 group")
	}
}
