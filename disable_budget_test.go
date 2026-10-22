// disable_budget_test.go — key 维度"成功 N 次后自动禁用"预算测试 + bug1/bug2 回归测试。
//
// 覆盖:
//   - bug1: 置顶失效后重新置顶有效(覆盖旧规则;不再被惰性残留的旧置顶挡住)
//   - bug2: 多个置顶成员轮询(rotateStrings 单元 + 群组路由 E2E 交替)
//   - 预算: toggle reqs=N 设置、maybeAutoDisable 达标触发、未达标不触发
//   - 立即禁用清预算、编辑保留预算、DisableNotes 渲染

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- bug1: 失效后重新置顶 ---

func TestGroupPin_RepinAfterExpiry(t *testing.T) {
	ks := newKeyStore()
	ks.configs["pa"] = KeyConfig{Key: "sk-1", Header: "Authorization"}
	// 已失效的置顶:规则在过去(惰性残留,activePins 已过滤)
	ks.groups = map[string]GroupConfig{
		"pg": {
			Members:  []string{"pa"},
			Pinned:   []string{"pa"},
			PinRules: map[string]PinRule{"pa": {Until: "2020-01-01 00:00"}},
			OnStatus: []int{429},
			Cooldown: "1m",
		},
	}
	ks.groupMgr.updateGroups(ks.groups)
	admin := &adminServer{keys: ks}

	if pins, _ := ks.groupMgr.activePins("pg"); len(pins) != 0 {
		t.Fatalf("前置:过期置顶应不生效, got %v", pins)
	}

	// 点"置顶"(带新时长):应替换旧规则、保持置顶,而不是走取消分支
	form := url.Values{"duration": {"1h"}}
	req := httptest.NewRequest("POST", "/__admin/groups/pin?name=pg&alias=pa", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleGroupPin(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("重新置顶应重定向 303, got %d: %s", w.Code, w.Body.String())
	}

	g := ks.groups["pg"]
	rule, ok := g.PinRules["pa"]
	if !ok {
		t.Fatal("重新置顶后规则应存在(旧过期规则被覆盖)")
	}
	exp, parseOK := parseExpires(rule.Until)
	if !parseOK || time.Until(exp) < 50*time.Minute {
		t.Errorf("新规则的 Until 应约 1 小时后, got %q", rule.Until)
	}
	pins, _ := ks.groupMgr.activePins("pg")
	if len(pins) != 1 || pins[0] != "pa" {
		t.Errorf("重新置顶后应生效, got %v", pins)
	}

	// 生效中再点一次(不带参数):应为取消置顶
	req2 := httptest.NewRequest("POST", "/__admin/groups/pin?name=pg&alias=pa", nil)
	w2 := httptest.NewRecorder()
	admin.handleGroupPin(w2, req2)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("取消置顶应重定向 303, got %d", w2.Code)
	}
	pins, _ = ks.groupMgr.activePins("pg")
	if len(pins) != 0 {
		t.Errorf("取消置顶后应无生效置顶, got %v", pins)
	}
}

// --- bug2: 多置顶轮询 ---

func TestRotateStrings(t *testing.T) {
	cases := []struct {
		in     []string
		offset int64
		want   []string
	}{
		{[]string{"a", "b", "c"}, 0, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c"}, 1, []string{"b", "c", "a"}},
		{[]string{"a", "b", "c"}, 3, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c"}, 5, []string{"c", "a", "b"}},
		{[]string{"a"}, 9, []string{"a"}},
		{nil, 1, nil},
	}
	for _, c := range cases {
		got := rotateStrings(c.in, c.offset)
		if len(got) != len(c.want) {
			t.Fatalf("rotate(%v,%d)=%v, want %v", c.in, c.offset, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("rotate(%v,%d)=%v, want %v", c.in, c.offset, got, c.want)
			}
		}
	}
}

func TestGroupRoute_PinnedMembersRotate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	defer setupGroupDisableEnv(t)()

	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["r1"] = KeyConfig{Key: "sk-1", Header: "Authorization", Prefix: "Bearer "}
	ks.configs["r2"] = KeyConfig{Key: "sk-2", Header: "Authorization", Prefix: "Bearer "}
	// 两个成员都置顶(无规则=永久),群组只有这两个成员
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"r1", "r2"}, Pinned: []string{"r1", "r2"}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	stats := newStatsCollector()
	us := newUsageStats()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleGroupRoute(w, r, ks, stats, us, "pool", backend.URL+"/v1/test")
	}))
	defer proxy.Close()

	// echoBackend 回显请求头,通过 Authorization 区分实际服务的成员
	servedKey := func() string {
		resp, err := http.Post(proxy.URL+"/g/pool/x", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("应 200, got %d", resp.StatusCode)
		}
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		s := string(buf[:n])
		if strings.Contains(s, `"Bearer sk-1"`) {
			return "sk-1"
		}
		if strings.Contains(s, `"Bearer sk-2"`) {
			return "sk-2"
		}
		t.Fatalf("响应不含可识别的 key 回显: %s", s)
		return ""
	}

	first := servedKey()
	second := servedKey()
	if first == "" || second == "" || first == second {
		t.Errorf("两个置顶成员应轮询交替: 第1次=%s 第2次=%s", first, second)
	}
}

// --- 禁用预算 ---

func TestMaybeAutoDisable(t *testing.T) {
	dir := t.TempDir()
	ks := newKeyStore()
	ks.path = filepath.Join(dir, "keys.yaml") // 触发真实落盘路径
	ks.setConfig("bt", KeyConfig{Key: "sk-1", Header: "Authorization", DisableAfterReqs: 5, DisableBaseReqs: 0})

	us := newUsageStats()
	// 4 次:未达标
	for i := 0; i < 4; i++ {
		us.recordSuccess("bt")
	}
	maybeAutoDisable(ks, us, "bt")
	cfg, ok := ks.lookup("bt")
	if !ok || cfg.Disabled {
		t.Fatal("4/5 次不应触发自动禁用")
	}

	// 第 5 次:达标
	us.recordSuccess("bt")
	maybeAutoDisable(ks, us, "bt")
	cfg, ok = ks.lookup("bt")
	if !ok {
		t.Fatal("bt 应存在")
	}
	if !cfg.Disabled {
		t.Fatal("5/5 次应触发自动禁用")
	}
	if cfg.DisableAfterReqs != 0 || cfg.DisableBaseReqs != 0 {
		t.Errorf("触发后预算字段应清零, got %+v", cfg)
	}
}

func TestMaybeAutoDisable_NoBudget(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("nb", KeyConfig{Key: "sk-1", Header: "Authorization"})
	us := newUsageStats()
	us.recordSuccess("nb")
	maybeAutoDisable(ks, us, "nb") // 无预算:不应 panic 不应禁用
	if cfg, _ := ks.lookup("nb"); cfg.Disabled {
		t.Fatal("无预算不应禁用")
	}
}

func TestHandleKeyToggle_ReqsBudget(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("bd", KeyConfig{Key: "sk-1", Header: "Authorization"})
	us := newUsageStats()
	us.recordSuccess("bd")
	us.recordSuccess("bd") // 基线 = 2
	admin := &adminServer{keys: ks, usage: us}

	// 设置预算:成功 10 次后禁用
	form := url.Values{"reqs": {"10"}}
	req := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=bd", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleKeyToggle(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("设置预算应重定向 303, got %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := ks.lookup("bd")
	if cfg.Disabled {
		t.Error("设置预算不应立即禁用")
	}
	if cfg.DisableAfterReqs != 10 || cfg.DisableBaseReqs != 2 {
		t.Errorf("预算字段不对: %+v", cfg)
	}

	// 达标触发
	for i := 0; i < 10; i++ {
		us.recordSuccess("bd")
	}
	maybeAutoDisable(ks, us, "bd")
	cfg, _ = ks.lookup("bd")
	if !cfg.Disabled || cfg.DisableAfterReqs != 0 {
		t.Errorf("达标后应自动禁用并清预算, got %+v", cfg)
	}

	// 立即禁用再设置:预算模式应解除禁用并重设预算
	form2 := url.Values{"reqs": {"20"}}
	req2 := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=bd", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	admin.handleKeyToggle(w2, req2)
	cfg, _ = ks.lookup("bd")
	if cfg.Disabled || cfg.DisableAfterReqs != 20 {
		t.Errorf("重设预算应保持启用, got %+v", cfg)
	}

	// 立即禁用:清掉预算
	req3 := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=bd", nil)
	w3 := httptest.NewRecorder()
	admin.handleKeyToggle(w3, req3)
	cfg, _ = ks.lookup("bd")
	if !cfg.Disabled || cfg.DisableAfterReqs != 0 || cfg.DisableBaseReqs != 0 {
		t.Errorf("立即禁用应清预算, got %+v", cfg)
	}
}

func TestHandleKeyNew_PreservesDisableBudget(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("bp", KeyConfig{Key: "sk-old", Header: "Authorization", DisableAfterReqs: 30, DisableBaseReqs: 7})
	admin := &adminServer{keys: ks, usage: newUsageStats()}

	form := url.Values{"alias": {"bp"}, "key": {""}, "header": {""}, "prefix": {""}, "rate": {"0"}, "burst": {"0"}, "expires": {""}}
	req := httptest.NewRequest("POST", "/__admin/keys/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleKeyNew(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("编辑保存应重定向 303, got %d", w.Code)
	}
	cfg, _ := ks.lookup("bp")
	if cfg.DisableAfterReqs != 30 || cfg.DisableBaseReqs != 7 {
		t.Errorf("编辑保存应保留预算字段, got %+v", cfg)
	}
}

func TestKeysPage_RendersDisableBudgetNote(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("va", KeyConfig{Key: "sk-1", Header: "Authorization", DisableAfterReqs: 50, DisableBaseReqs: 0})
	us := newUsageStats()
	us.recordSuccess("va") // 用了 1 次,剩 49
	admin := &adminServer{keys: ks, usage: us}

	req := httptest.NewRequest("GET", "/__admin/keys", nil)
	w := httptest.NewRecorder()
	admin.handleKeys(w, req)
	body := w.Body.String()
	for _, want := range []string{"剩49次后自动禁用", "预算中", `value="100">100次后`} {
		if !strings.Contains(body, want) {
			t.Errorf("Keys 页应包含 %q", want)
		}
	}
}
