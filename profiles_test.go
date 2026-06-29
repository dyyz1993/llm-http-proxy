// profiles_test.go — 拦截器模板(Profiles)管理界面测试
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestProfilesGET 验证 Profiles 页 GET 渲染。
func TestProfilesGET(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	initialYAML := `interceptor_profiles:
  default:
    rate: 60
    burst: 10
    max_tokens: 10000000
  night_block:
    rate: 30
    max_requests: 2000
    window: 12h
    time_block:
      start: "22:00"
      end: "08:00"
glm:
  key: "test-key"
`
	if err := os.WriteFile(yamlPath, []byte(initialYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ks := newKeyStore()
	if err := ks.load(yamlPath); err != nil {
		t.Fatal(err)
	}

	admin := &adminServer{
		password: "test",
		secret:   make([]byte, 32),
		stats:    newStatsCollector(),
		keys:     ks,
		settings: newSettingsManager(),
	}
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	req := httptest.NewRequest("GET", "/__admin/profiles", nil)
	w := httptest.NewRecorder()
	admin.handleProfiles(w, req)

	resp := w.Result()
	body := make([]byte, 8192)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET 应返回 200, 得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "default") {
		t.Error("页面应包含 default 模板")
	}
	if !strings.Contains(bodyStr, "night_block") {
		t.Error("页面应包含 night_block 模板")
	}
	if !strings.Contains(bodyStr, "22:00") {
		t.Error("页面应包含 time_block 信息")
	}
	if !strings.Contains(bodyStr, "新增模板") {
		t.Error("页面应包含新增表单")
	}
}

// TestProfilesPOST 验证新增/编辑模板。
func TestProfilesPOST(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	initialYAML := `glm:
  key: "test"
`
	if err := os.WriteFile(yamlPath, []byte(initialYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ks := newKeyStore()
	if err := ks.load(yamlPath); err != nil {
		t.Fatal(err)
	}

	admin := &adminServer{
		password: "test",
		secret:   make([]byte, 32),
		stats:    newStatsCollector(),
		keys:     ks,
		settings: newSettingsManager(),
	}
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	// POST: 新建模板
	form := url.Values{
		"name":             {"strict"},
		"rate":             {"20"},
		"burst":            {"5"},
		"max_tokens":       {"1000000"},
		"max_requests":     {"500"},
		"window":           {"1h"},
		"time_block_start": {"23:00"},
		"time_block_end":   {"07:00"},
	}
	req := httptest.NewRequest("POST", "/__admin/profiles/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleProfileNew(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		t.Errorf("POST 应 302 跳转, 得到 %d, body: %s", resp.StatusCode, string(body[:n]))
	}
	resp.Body.Close()

	// 验证已保存到内存
	profiles := ks.allProfiles()
	strict, ok := profiles["strict"]
	if !ok {
		t.Fatal("strict 模板应存在")
	}
	if strict.Rate != 20 {
		t.Errorf("rate 应为 20, 得到 %d", strict.Rate)
	}
	if strict.Burst != 5 {
		t.Errorf("burst 应为 5, 得到 %d", strict.Burst)
	}
	if strict.MaxTokens != 1000000 {
		t.Errorf("max_tokens 应为 1000000, 得到 %d", strict.MaxTokens)
	}
	if strict.MaxReqs != 500 {
		t.Errorf("max_requests 应为 500, 得到 %d", strict.MaxReqs)
	}
	if strict.Window != "1h" {
		t.Errorf("window 应为 1h, 得到 %s", strict.Window)
	}
	if strict.TimeBlock == nil {
		t.Fatal("time_block 不应为空")
	}
	if strict.TimeBlock.Start != "23:00" {
		t.Errorf("time_block.start 应为 23:00, 得到 %s", strict.TimeBlock.Start)
	}
	if strict.TimeBlock.End != "07:00" {
		t.Errorf("time_block.end 应为 07:00, 得到 %s", strict.TimeBlock.End)
	}

	// 验证持久化到文件(全局段 + alias 都保留)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "interceptor_profiles:") {
		t.Error("文件应包含 interceptor_profiles 段")
	}
	if !strings.Contains(content, "strict:") {
		t.Error("文件应包含 strict 模板")
	}
	if !strings.Contains(content, "glm:") {
		t.Error("文件应保留原有的 alias 配置")
	}
}

// TestProfilesDelete 验证删除模板。
func TestProfilesDelete(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	initialYAML := `interceptor_profiles:
  default:
    rate: 60
  to_delete:
    rate: 10
glm:
  key: "test"
`
	if err := os.WriteFile(yamlPath, []byte(initialYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ks := newKeyStore()
	if err := ks.load(yamlPath); err != nil {
		t.Fatal(err)
	}

	if len(ks.allProfiles()) != 2 {
		t.Fatal("应有 2 个模板")
	}

	admin := &adminServer{
		password: "test",
		secret:   make([]byte, 32),
		stats:    newStatsCollector(),
		keys:     ks,
		settings: newSettingsManager(),
	}
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	// POST: 删除模板
	req := httptest.NewRequest("POST", "/__admin/profiles/delete?name=to_delete", nil)
	w := httptest.NewRecorder()
	admin.handleProfileDelete(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		t.Errorf("DELETE 应 302 跳转, 得到 %d, body: %s", resp.StatusCode, string(body[:n]))
	}
	resp.Body.Close()

	// 验证已删除
	profiles := ks.allProfiles()
	if len(profiles) != 1 {
		t.Errorf("删除后应有 1 个模板, 得到 %d", len(profiles))
	}
	if _, ok := profiles["to_delete"]; ok {
		t.Error("to_delete 应已被删除")
	}
	if _, ok := profiles["default"]; !ok {
		t.Error("default 应保留")
	}
}

// TestProfilesNoKeys 验证未启用 key 模式时给出提示。
func TestProfilesNoKeys(t *testing.T) {
	admin := &adminServer{
		password: "test",
		secret:   make([]byte, 32),
		stats:    newStatsCollector(),
		keys:     nil,
		settings: newSettingsManager(),
	}
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	req := httptest.NewRequest("GET", "/__admin/profiles", nil)
	w := httptest.NewRecorder()
	admin.handleProfiles(w, req)

	resp := w.Result()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("应返回 200, 得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "Key 注入模式未启用") {
		t.Error("应提示 key 模式未启用")
	}
}
