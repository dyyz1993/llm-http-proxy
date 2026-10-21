// pin_expiry_test.go — 置顶自动解除(PinRule:时长 / 成功请求数)测试。
//
// 覆盖:
//   - YAML 往返:pin_rules 加载/保存
//   - activePins:时长到期过滤、成功请求数达标过滤、剩余说明、永久置顶
//   - handleGroupPin:带时长/次数参数置顶、取消清理规则、非法时长报错
//   - 群组编辑保存保留规则(filterPinRules)
//   - 群组页模板渲染置顶表单与剩余说明

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- YAML 往返 ---

func TestPinRule_YAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	content := `groups:
    pool:
        members:
            - ma
            - mb
        pinned:
            - ma
        pin_rules:
            ma:
                until: "2099-01-01 12:00"
                max_requests: 100
                base_requests: 40
        on_status: [429]
        cooldown: 1m
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ks := newKeyStore()
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}
	g, ok := ks.groups["pool"]
	if !ok {
		t.Fatal("pool 应存在")
	}
	if len(g.Pinned) != 1 || g.Pinned[0] != "ma" {
		t.Fatalf("pinned 应为 [ma], got %v", g.Pinned)
	}
	rule, ok := g.PinRules["ma"]
	if !ok {
		t.Fatal("pin_rules[ma] 应存在")
	}
	if rule.Until != "2099-01-01 12:00" || rule.MaxReqs != 100 || rule.BaseReqs != 40 {
		t.Errorf("规则字段不对: %+v", rule)
	}

	// 保存后再读回,规则不丢
	if err := ks.setGroup("pool", g); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"pin_rules", "until: 2099-01-01 12:00", "max_requests: 100", "base_requests: 40"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("保存后的文件应包含 %q", want)
		}
	}
}

// --- activePins 判定 ---

func TestActivePins_TimeExpiry(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {
			Members:  []string{"ma", "mb"},
			Pinned:   []string{"ma"},
			PinRules: map[string]PinRule{"ma": {Until: "2000-01-01 00:00"}}, // 远古过去
		},
	})

	pins, _ := gm.activePins("pool")
	if len(pins) != 0 {
		t.Errorf("到期的置顶应被过滤, got %v", pins)
	}

	// 未来时间:生效且有剩余说明
	gm.updateGroups(map[string]GroupConfig{
		"pool": {
			Members:  []string{"ma", "mb"},
			Pinned:   []string{"ma"},
			PinRules: map[string]PinRule{"ma": {Until: time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04")}},
		},
	})
	pins, notes := gm.activePins("pool")
	if len(pins) != 1 || pins[0] != "ma" {
		t.Fatalf("未到期的置顶应生效, got %v", pins)
	}
	if !strings.Contains(notes["ma"], "剩") {
		t.Errorf("应有时长剩余说明, got %q", notes["ma"])
	}

	// 无规则(永久置顶):生效且无说明
	gm.updateGroups(map[string]GroupConfig{
		"pool": {Members: []string{"ma", "mb"}, Pinned: []string{"ma"}},
	})
	pins, notes = gm.activePins("pool")
	if len(pins) != 1 {
		t.Fatalf("永久置顶应生效, got %v", pins)
	}
	if notes["ma"] != "" {
		t.Errorf("永久置顶不应有剩余说明, got %q", notes["ma"])
	}
}

func TestActivePins_ReqCount(t *testing.T) {
	gm := newGroupManager()
	gm.updateGroups(map[string]GroupConfig{
		"pool": {
			Members:  []string{"ma", "mb"},
			Pinned:   []string{"ma"},
			PinRules: map[string]PinRule{"ma": {MaxReqs: 5, BaseReqs: 0}},
		},
	})
	// 模拟成功 3 次 + 失败 2 次:成功数 3 < 5,仍生效,剩 2 次
	gm.states["ma"] = &memberState{
		calias:       "ma",
		totalReqs:    5,
		statusCounts: map[int]int64{200: 3, 429: 2},
	}
	pins, notes := gm.activePins("pool")
	if len(pins) != 1 {
		t.Fatalf("成功 3/5 应仍生效, got %v", pins)
	}
	if !strings.Contains(notes["ma"], "剩2次") {
		t.Errorf("应剩 2 次, got %q", notes["ma"])
	}

	// 成功达到 5 次(失败不计入):自动解除
	gm.states["ma"].totalReqs = 7
	gm.states["ma"].statusCounts = map[int]int64{200: 5, 429: 2}
	pins, _ = gm.activePins("pool")
	if len(pins) != 0 {
		t.Errorf("成功达 5/5 应自动解除, got %v", pins)
	}
}

// --- handleGroupPin 参数 ---

func TestHandleGroupPin_WithDurationAndReqs(t *testing.T) {
	ks := newKeyStore()
	ks.configs["pa"] = KeyConfig{Key: "sk-1", Header: "Authorization"}
	ks.groups = map[string]GroupConfig{
		"pg": {Members: []string{"pa"}, OnStatus: []int{429}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)
	// 预置一些成功请求,验证基线
	ks.groupMgr.markSuccess("pa", 200)
	ks.groupMgr.markSuccess("pa", 200)
	admin := &adminServer{keys: ks}

	form := url.Values{"duration": {"1h"}, "maxreqs": {"10"}}
	req := httptest.NewRequest("POST", "/__admin/groups/pin?name=pg&alias=pa", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleGroupPin(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("置顶应重定向 303, got %d: %s", w.Code, w.Body.String())
	}

	g := ks.groups["pg"]
	if !sliceContains(g.Pinned, "pa") {
		t.Fatal("pa 应已被置顶")
	}
	rule, ok := g.PinRules["pa"]
	if !ok {
		t.Fatal("pin_rules[pa] 应存在")
	}
	if rule.MaxReqs != 10 {
		t.Errorf("MaxReqs 应为 10, got %d", rule.MaxReqs)
	}
	if rule.BaseReqs != 2 {
		t.Errorf("BaseReqs 应为当前成功数 2, got %d", rule.BaseReqs)
	}
	exp, ok := parseExpires(rule.Until)
	if !ok {
		t.Fatalf("Until 应可解析: %q", rule.Until)
	}
	d := time.Until(exp)
	if d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("Until 应约为 1 小时后, 实际 %v", d)
	}

	// 取消置顶:规则一并清理
	req2 := httptest.NewRequest("POST", "/__admin/groups/pin?name=pg&alias=pa", nil)
	w2 := httptest.NewRecorder()
	admin.handleGroupPin(w2, req2)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("取消置顶应重定向 303, got %d", w2.Code)
	}
	g = ks.groups["pg"]
	if sliceContains(g.Pinned, "pa") {
		t.Error("pa 应已被取消置顶")
	}
	if len(g.PinRules) != 0 {
		t.Errorf("取消置顶应清理规则, got %v", g.PinRules)
	}
}

func TestHandleGroupPin_InvalidDuration(t *testing.T) {
	ks := newKeyStore()
	ks.configs["pa"] = KeyConfig{Key: "sk-1", Header: "Authorization"}
	ks.groups = map[string]GroupConfig{
		"pg": {Members: []string{"pa"}, OnStatus: []int{429}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)
	admin := &adminServer{keys: ks}

	form := url.Values{"duration": {"not-a-duration"}}
	req := httptest.NewRequest("POST", "/__admin/groups/pin?name=pg&alias=pa", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleGroupPin(w, req)

	g := ks.groups["pg"]
	if sliceContains(g.Pinned, "pa") {
		t.Error("非法时长不应置顶")
	}
}

// --- 群组编辑保存保留规则 ---

func TestFilterPinRules(t *testing.T) {
	rules := map[string]PinRule{
		"keep": {MaxReqs: 5},
		"gone": {MaxReqs: 6},
	}
	out := filterPinRules(rules, []string{"keep"})
	if len(out) != 1 || out["keep"].MaxReqs != 5 {
		t.Errorf("应只保留 keep 的规则, got %v", out)
	}
	if filterPinRules(nil, []string{"a"}) != nil {
		t.Error("nil 规则应返回 nil")
	}
	if filterPinRules(rules, nil) != nil {
		t.Error("置顶为空应返回 nil")
	}
}

func TestHandleGroupNew_PreservesPinRules(t *testing.T) {
	ks := newKeyStore()
	ks.configs["pa"] = KeyConfig{Key: "sk-1", Header: "Authorization"}
	ks.groups = map[string]GroupConfig{
		"pg": {
			Members:  []string{"pa", "pb"},
			Pinned:   []string{"pa"},
			PinRules: map[string]PinRule{"pa": {MaxReqs: 5}, "px": {MaxReqs: 9}},
			OnStatus: []int{429},
			Cooldown: "1m",
		},
	}
	ks.groupMgr.updateGroups(ks.groups)
	admin := &adminServer{keys: ks}

	form := url.Values{"name": {"pg"}, "members": {"pa,pb"}, "on_status": {"429"}, "cooldown": {"1m"}}
	req := httptest.NewRequest("POST", "/__admin/groups/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleGroupNew(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("保存应重定向 303, got %d: %s", w.Code, w.Body.String())
	}

	g := ks.groups["pg"]
	if len(g.Pinned) != 1 || g.Pinned[0] != "pa" {
		t.Errorf("编辑保存应保留置顶, got %v", g.Pinned)
	}
	if len(g.PinRules) != 1 || g.PinRules["pa"].MaxReqs != 5 {
		t.Errorf("编辑保存应保留置顶成员的规则并剔除已移除成员的, got %v", g.PinRules)
	}
}

// --- 模板渲染 ---

func TestGroupsPage_RendersPinFormAndNotes(t *testing.T) {
	ks := newKeyStore()
	ks.configs["pm1"] = KeyConfig{Key: "sk-1", Header: "Authorization"} // 永久置顶
	ks.configs["pm2"] = KeyConfig{Key: "sk-2", Header: "Authorization"} // 限时置顶
	ks.configs["pm3"] = KeyConfig{Key: "sk-3", Header: "Authorization"} // 未置顶
	ks.groups = map[string]GroupConfig{
		"pg": {
			Members: []string{"pm1", "pm2", "pm3"},
			Pinned:  []string{"pm1", "pm2"},
			PinRules: map[string]PinRule{
				// 同时设时长+次数:剩余说明应是"或"的关系
				"pm2": {
					Until:   time.Now().Add(30 * time.Minute).Format("2006-01-02 15:04"),
					MaxReqs: 100,
				},
			},
			OnStatus: []int{429},
			Cooldown: "1m",
		},
	}
	ks.groupMgr.updateGroups(ks.groups)
	admin := &adminServer{keys: ks}

	req := httptest.NewRequest("GET", "/__admin/groups", nil)
	w := httptest.NewRecorder()
	admin.handleGroups(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("群组页应 200, got %d", w.Code)
	}
	for _, want := range []string{
		`<details class="pin-picker">`,     // 未置顶成员:点"置顶"展开面板
		`name="duration" value="" checked`, // 默认选中"不限时"
		`name="maxreqs" value="0" checked`, // 默认选中"不限次"
		"30分钟",                             // 预设选项
		"100次",                             // 预设选项
		">确认</button>",                     // 面板确认按钮
		">取消</button>",                     // 面板取消按钮
		"剩",                                // pm2 的剩余说明
		" 或 ",                              // 同时设时长+次数时明确"或"关系
		"取消置顶",                             // 已置顶成员(pm1/pm2)的按钮
	} {
		if !strings.Contains(body, want) {
			t.Errorf("群组页应包含 %q", want)
		}
	}
}
