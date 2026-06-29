// config_test.go — YAML 配置编辑器测试
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestConfigGET 验证 YAML 配置页 GET 能正常渲染。
func TestConfigGET(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	// 写入初始化 YAML
	initialYAML := `glm:
  key: "test-key-123"
  rate: 30

interceptor_profiles:
  default:
    rate: 60
    max_tokens: 10000000
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

	req := httptest.NewRequest("GET", "/__admin/config", nil)
	w := httptest.NewRecorder()
	admin.handleConfig(w, req)

	resp := w.Result()
	body := make([]byte, 8192)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET 应返回 200, 得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "test-key-123") {
		t.Error("页面应包含 YAML 中的 key 内容")
	}
	if !strings.Contains(bodyStr, "interceptor_profiles") {
		t.Error("页面应包含全局段 interceptor_profiles")
	}
	if !strings.Contains(bodyStr, "text/yaml") && !strings.Contains(bodyStr, "textarea") {
		// 检查是否有 textarea 编辑器
		if !strings.Contains(bodyStr, "textarea") {
			t.Error("页面应包含 textarea 编辑器")
		}
	}
	if !strings.Contains(bodyStr, "保存配置") {
		t.Error("页面应包含保存按钮")
	}
}

// TestConfigPOST 验证 YAML 保存功能。
func TestConfigPOST(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	initialYAML := `glm:
  key: "old-key"
  rate: 10
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

	// POST: 提交新 YAML
	newYAML := `glm:
  key: "new-key"
  rate: 50

interceptor_profiles:
  default:
    rate: 30
    max_tokens: 5000000
`
	form := url.Values{"yaml": {newYAML}}
	req := httptest.NewRequest("POST", "/__admin/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleConfig(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		t.Errorf("POST 应返回 200(渲染 msg 页), 得到 %d, body: %s", resp.StatusCode, string(body[:n]))
	} else {
		resp.Body.Close()
	}

	// 验证文件已更新
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new-key") {
		t.Error("文件应包含新 key")
	}
	if !strings.Contains(string(data), "interceptor_profiles") {
		t.Error("文件应包含全局段 interceptor_profiles")
	}
	if strings.Contains(string(data), "old-key") {
		t.Error("文件不应再包含旧 key")
	}

	// 验证 keyStore 已重新加载
	cfg, ok := ks.lookup("glm")
	if !ok {
		t.Fatal("glm 应存在")
	}
	if cfg.Key != "new-key" {
		t.Errorf("key 应为 new-key, 得到 %s", cfg.Key)
	}
	// 检查全局段是否加载
	profiles := ks.getInterceptorProfiles()
	if profiles == nil {
		t.Error("interceptor_profiles 应已加载")
	} else {
		def, ok := profiles["default"]
		if !ok {
			t.Error("default profile 应存在")
		} else if def.Rate != 30 {
			t.Errorf("default profile rate 应=30, 得到 %d", def.Rate)
		}
	}
}

// TestConfigPOSTInvalidYAML 验证无效 YAML 保存会返回错误。
func TestConfigPOSTInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := tmpDir + "/keys.yaml"

	initialYAML := `glm:
  key: "test-key"
`
	if err := os.WriteFile(yamlPath, []byte(initialYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ks := newKeyStore()
	if err := ks.load(yamlPath); err != nil {
		t.Fatal(err)
	}
	originalConfigs := ks.allConfigs()

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

	// POST 无效 YAML
	badYAML := `glm:
  key: "test"
  rate: :invalid
`
	form := url.Values{"yaml": {badYAML}}
	req := httptest.NewRequest("POST", "/__admin/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	admin.handleConfig(w, req)

	resp := w.Result()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("无效 YAML 应返回 200(渲染错误页), 得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "YAML 语法错误") {
		t.Error("应显示 YAML 语法错误信息")
	}
	// textarea 应保留用户输入
	if !strings.Contains(bodyStr, ":invalid") {
		t.Error("textarea 应保留用户输入的无效内容以便修正")
	}

	// 旧配置不应受影响
	cfg, ok := ks.lookup("glm")
	if !ok {
		t.Fatal("glm 应仍存在")
	}
	if cfg.Key != "test-key" {
		t.Errorf("key 不应改变, 得到 %s", cfg.Key)
	}

	// 检查 originalConfigs 和 current 一样
	current := ks.allConfigs()
	if len(current) != len(originalConfigs) {
		t.Error("配置数量不应改变")
	}
}

// TestConfigNoKeys 验证未启用 key 模式时 config 页给出提示。
func TestConfigNoKeys(t *testing.T) {
	admin := &adminServer{
		password: "test",
		secret:   make([]byte, 32),
		stats:    newStatsCollector(),
		keys:     nil, // key 模式未启用
		settings: newSettingsManager(),
	}
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	req := httptest.NewRequest("GET", "/__admin/config", nil)
	w := httptest.NewRecorder()
	admin.handleConfig(w, req)

	resp := w.Result()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("未启用时应返回 200, 得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "Key 注入模式未启用") {
		t.Error("应提示 key 模式未启用")
	}
}
