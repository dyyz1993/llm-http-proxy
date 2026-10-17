// keys_disable_test.go — key 维度手动禁用(quick disable)测试。
//
// 覆盖:
//   - YAML 往返:disabled: true 加载/保存/清除(omitempty)
//   - 直连 /k/{alias}/:禁用 → 503,启用恢复 → 200
//   - 群组路由:禁用成员被跳过(不冷却、不计数);全员禁用 → 503
//   - toggle handler:翻转 + 持久化落盘
//   - 编辑保存(handleKeyNew)不丢失禁用状态
//   - computeMemberHealth 返回 "disabled"
//   - Keys 页 / 群组页模板渲染出禁用按钮和徽标(模板语法兜底)

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
)

// --- YAML 往返 ---

func TestKeyDisabled_YAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	content := `off:
    key: sk-abc
    header: Authorization
    disabled: true
on:
    key: sk-def
    header: Authorization
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ks := newKeyStore()
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}
	cfg, ok := ks.lookup("off")
	if !ok {
		t.Fatal("off 应存在且未过期")
	}
	if !cfg.Disabled {
		t.Error("off 加载后应为禁用")
	}
	cfgOn, ok := ks.lookup("on")
	if !ok {
		t.Fatal("on 应存在")
	}
	if cfgOn.Disabled {
		t.Error("on 不应为禁用")
	}

	// 禁用 → 启用:文件不应再有 disabled: true(omitempty)
	cfg.Disabled = false
	if err := ks.setConfig("off", cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("disabled: true")) {
		t.Error("启用后文件不应再有 disabled: true")
	}

	// 再禁用:文件应有 disabled: true
	cfg.Disabled = true
	if err := ks.setConfig("off", cfg); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("disabled: true")) {
		t.Error("禁用后文件应有 disabled: true")
	}
}

// --- 直连 /k/{alias}/ ---

func TestHandleKeyRoute_DisabledAlias(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.setConfig("dk", KeyConfig{Key: "sk-test", Header: "Authorization", Disabled: true})

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

	resp, err := http.Get(proxy.URL + "/k/dk/" + backend.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("禁用的 alias 直连应返回 503, got %d", resp.StatusCode)
	}

	// 启用后恢复 200
	cfg, _ := ks.lookup("dk")
	cfg.Disabled = false
	ks.setConfig("dk", cfg)

	resp2, err := http.Get(proxy.URL + "/k/dk/" + backend.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("启用后直连应恢复 200, got %d", resp2.StatusCode)
	}
}

// --- 群组路由:跳过禁用成员 ---

func setupGroupDisableEnv(t *testing.T) (restore func()) {
	t.Helper()
	oldSettings := settingsMgr
	oldTracker := usageTracker
	settingsMgr = newSettingsManager()
	usageTracker = newUsageStats()
	return func() {
		settingsMgr = oldSettings
		usageTracker = oldTracker
	}
}

func TestGroupRoute_SkipsDisabledMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	defer setupGroupDisableEnv(t)()

	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-1", Header: "Authorization", Disabled: true}
	ks.configs["m2"] = KeyConfig{Key: "sk-2", Header: "Authorization"}
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1", "m2"}, OnStatus: []int{402, 429, 502, 503}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	stats := newStatsCollector()
	us := newUsageStats()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleGroupRoute(w, r, ks, stats, us, "pool", backend.URL+"/v1/test")
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/g/pool/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("m1 禁用时应由 m2 接管返回 200, got %d", resp.StatusCode)
	}

	// 禁用成员不应被尝试:无冷却、无计数
	st := ks.groupMgr.memberStatus("m1")
	if st.IsCooling {
		t.Error("禁用成员不应进冷却(它是人为开关,不是故障)")
	}
	if st.TotalReqs != 0 {
		t.Errorf("禁用成员不应被计入请求, got %d", st.TotalReqs)
	}
}

func TestGroupRoute_AllMembersDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	defer setupGroupDisableEnv(t)()

	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-1", Header: "Authorization", Disabled: true}
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1"}, OnStatus: []int{402, 429, 502, 503}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)

	stats := newStatsCollector()
	us := newUsageStats()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleGroupRoute(w, r, ks, stats, us, "pool", backend.URL+"/v1/test")
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/g/pool/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("全员禁用应返回 503, got %d", resp.StatusCode)
	}
}

// --- toggle handler:翻转 + 持久化 ---

func TestHandleKeyToggle_FlipsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	if err := os.WriteFile(path, []byte("tk:\n    key: sk-xyz\n    header: Authorization\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ks := newKeyStore()
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}
	admin := &adminServer{keys: ks}

	// 禁用
	req := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=tk", nil)
	w := httptest.NewRecorder()
	admin.handleKeyToggle(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("toggle 应重定向 303, got %d", w.Code)
	}
	cfg, ok := ks.lookup("tk")
	if !ok || !cfg.Disabled {
		t.Fatal("toggle 后 tk 应为禁用")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte("disabled: true")) {
		t.Error("禁用后应落盘 disabled: true")
	}

	// 再启用
	req2 := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=tk", nil)
	w2 := httptest.NewRecorder()
	admin.handleKeyToggle(w2, req2)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("toggle 应重定向 303, got %d", w2.Code)
	}
	cfg, ok = ks.lookup("tk")
	if !ok || cfg.Disabled {
		t.Fatal("二次 toggle 后 tk 应恢复启用")
	}
	raw, _ = os.ReadFile(path)
	if bytes.Contains(raw, []byte("disabled: true")) {
		t.Error("启用后文件不应再有 disabled: true")
	}
}

func TestHandleKeyToggle_UnknownAlias(t *testing.T) {
	ks := newKeyStore()
	admin := &adminServer{keys: ks}
	req := httptest.NewRequest("POST", "/__admin/keys/toggle?alias=nope", nil)
	w := httptest.NewRecorder()
	admin.handleKeyToggle(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("未知 alias 应 404, got %d", w.Code)
	}
}

// --- 编辑保存不丢失禁用状态 ---

func TestHandleKeyNew_PreservesDisabled(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("ep", KeyConfig{Key: "sk-old", Header: "Authorization", Disabled: true})
	admin := &adminServer{keys: ks}

	// 编辑:key 留空=保留原 key,其他字段照常
	form := url.Values{
		"alias":   {"ep"},
		"key":     {""},
		"header":  {""},
		"prefix":  {""},
		"rate":    {"0"},
		"burst":   {"0"},
		"expires": {""},
	}
	req := httptest.NewRequest("POST", "/__admin/keys/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleKeyNew(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("编辑保存应重定向 303, got %d: %s", w.Code, w.Body.String())
	}

	cfg, ok := ks.lookup("ep")
	if !ok {
		t.Fatal("ep 应仍存在")
	}
	if !cfg.Disabled {
		t.Error("编辑保存不应丢失禁用状态")
	}
	if cfg.Key != "sk-old" {
		t.Errorf("key 留空应保留原 key, got %q", cfg.Key)
	}
}

// --- 成员健康标签 ---

func TestComputeMemberHealth_Disabled(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("hm", KeyConfig{Key: "sk", Header: "Authorization", Disabled: true})
	if got := computeMemberHealth(ks, nil, "hm"); got != "disabled" {
		t.Errorf("禁用成员健康标签应为 disabled, got %q", got)
	}
	cfg, _ := ks.lookup("hm")
	cfg.Disabled = false
	ks.setConfig("hm", cfg)
	if got := computeMemberHealth(ks, nil, "hm"); got != "ok" {
		t.Errorf("启用后健康标签应为 ok, got %q", got)
	}
}

// --- 模板渲染(Keys 页 / 群组页) ---

func TestKeysPage_RendersToggle(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("vis", KeyConfig{Key: "sk-v", Header: "Authorization"})
	ks.setConfig("hid", KeyConfig{Key: "sk-h", Header: "Authorization", Disabled: true})
	admin := &adminServer{keys: ks}

	req := httptest.NewRequest("GET", "/__admin/keys", nil)
	w := httptest.NewRecorder()
	admin.handleKeys(w, req)
	body := w.Body.String()

	if w.Code != http.StatusOK {
		t.Fatalf("Keys 页应 200, got %d", w.Code)
	}
	for _, want := range []string{
		"/__admin/keys/toggle?alias=hid",
		"/__admin/keys/toggle?alias=vis",
		"⛔ 已禁用",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Keys 页应包含 %q", want)
		}
	}
}

func TestGroupsPage_RendersMemberToggle(t *testing.T) {
	ks := newKeyStore()
	ks.configs["gm1"] = KeyConfig{Key: "sk-1", Header: "Authorization", Disabled: true}
	ks.groups = map[string]GroupConfig{
		"pg": {Members: []string{"gm1"}, OnStatus: []int{429}, Cooldown: "1m"},
	}
	admin := &adminServer{keys: ks}

	req := httptest.NewRequest("GET", "/__admin/groups", nil)
	w := httptest.NewRecorder()
	admin.handleGroups(w, req)
	body := w.Body.String()

	if w.Code != http.StatusOK {
		t.Fatalf("群组页应 200, got %d", w.Code)
	}
	for _, want := range []string{
		"/__admin/keys/toggle?alias=gm1&back=groups",
		"⛔ 已禁用",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("群组页应包含 %q", want)
		}
	}
}
