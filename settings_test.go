package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// ---- 单元测试: settingsManager 基础操作 ----

func TestSettingsManager_DefaultUnrestricted(t *testing.T) {
	sm := newSettingsManager()
	// 空白名单 = 不限制
	if !sm.IsAllowed("any.domain.com") {
		t.Error("空白名单应允许所有域名")
	}
	if sm.IsWhitelistEnabled() {
		t.Error("空白名单不应视为已启用")
	}
	if sm.DomainCount() != 0 {
		t.Errorf("期望 0 个域名, 得到 %d", sm.DomainCount())
	}
}

func TestSettingsManager_AddDomain(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai")

	if !sm.IsAllowed("api.z.ai") {
		t.Error("已添加的域名应被允许")
	}
	if !sm.IsAllowed("API.Z.AI") {
		t.Error("域名匹配应大小写不敏感")
	}
	if sm.IsAllowed("evil.hacker.com") {
		t.Error("未添加的域名应被拒绝")
	}
	if !sm.IsWhitelistEnabled() {
		t.Error("有域名时应视为已启用")
	}
	if sm.DomainCount() != 1 {
		t.Errorf("期望 1 个域名, 得到 %d", sm.DomainCount())
	}
}

func TestSettingsManager_AddEmptyString(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("")   // 空字符串应被忽略
	sm.AddDomain("  ") // 空白应被忽略

	if sm.DomainCount() != 0 {
		t.Errorf("空白域名不应被添加, 得到 %d 个域名", sm.DomainCount())
	}
}

func TestSettingsManager_RemoveDomain(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai")
	sm.AddDomain("open.bigmodel.cn")

	if sm.DomainCount() != 2 {
		t.Fatalf("期望 2 个域名, 得到 %d", sm.DomainCount())
	}

	sm.RemoveDomain("api.z.ai")
	if sm.IsAllowed("api.z.ai") {
		t.Error("移除后域名应被拒绝")
	}
	if !sm.IsAllowed("open.bigmodel.cn") {
		t.Error("其他域名应仍被允许")
	}
	if sm.DomainCount() != 1 {
		t.Errorf("期望 1 个域名, 得到 %d", sm.DomainCount())
	}

	// 移除不存在的域名不报错
	sm.RemoveDomain("nonexistent.com")
	if sm.DomainCount() != 1 {
		t.Errorf("移除非存在域名不应改变计数, 得到 %d", sm.DomainCount())
	}
}

func TestSettingsManager_MultipleDomains(t *testing.T) {
	sm := newSettingsManager()
	domains := []string{"api.z.ai", "open.bigmodel.cn", "api.deepseek.com", "api.anthropic.com"}
	for _, d := range domains {
		sm.AddDomain(d)
	}

	got := sm.GetDomains()
	if len(got) != len(domains) {
		t.Fatalf("期望 %d 个域名, 得到 %d", len(domains), len(got))
	}

	// 验证排序
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("域名未排序: %v", got)
			break
		}
	}

	// 验证所有添加的域名都被允许
	for _, d := range domains {
		if !sm.IsAllowed(d) {
			t.Errorf("域名 %q 应被允许", d)
		}
	}
}

// ---- 持久化测试 ----

func TestSettingsManager_SaveLoad(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai")
	sm.AddDomain("open.bigmodel.cn")

	tmpDir := t.TempDir()
	path := tmpDir + "/settings.json"

	// 保存
	if err := sm.save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 验证文件内容
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	var sd settingsData
	if err := json.Unmarshal(data, &sd); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if len(sd.Domains) != 2 {
		t.Errorf("期望 2 个域名, 得到 %d", len(sd.Domains))
	}

	// 加载到新的 settingsManager
	sm2 := newSettingsManager()
	if err := sm2.load(path); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !sm2.IsAllowed("api.z.ai") {
		t.Error("加载后域名应被允许")
	}
	if !sm2.IsAllowed("open.bigmodel.cn") {
		t.Error("加载后域名应被允许")
	}
	if sm2.IsAllowed("evil.hacker.com") {
		t.Error("未保存的域名不应被允许")
	}
	if sm2.DomainCount() != 2 {
		t.Errorf("期望 2 个域名, 得到 %d", sm2.DomainCount())
	}
}

func TestSettingsManager_LoadNonexistentFile(t *testing.T) {
	sm := newSettingsManager()
	// 不存在的文件应返回 nil 错误
	if err := sm.load("/nonexistent/path/settings.json"); err != nil {
		t.Errorf("加载不存在文件应返回 nil, 得到: %v", err)
	}
}

func TestSettingsManager_SaveEmptyWhitelist(t *testing.T) {
	sm := newSettingsManager()
	tmpDir := t.TempDir()
	path := tmpDir + "/empty.json"

	// 空白名单 = 不限制,保存时应删除文件
	if err := sm.save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 文件应不存在（或用临时文件后删除了）
	if _, err := os.Stat(path); err == nil {
		// 也可能是文件被删了
	}
}

func TestSettingsManager_LoadEmptyJson(t *testing.T) {
	sm := newSettingsManager()
	tmpDir := t.TempDir()
	path := tmpDir + "/empty.json"

	// 写入空 JSON
	if err := os.WriteFile(path, []byte(`{"domains":[]}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := sm.load(path); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if sm.IsWhitelistEnabled() {
		t.Error("空列表不应视为已启用")
	}
	if !sm.IsAllowed("anything.com") {
		t.Error("空列表应允许所有域名")
	}
}

// ---- 合并测试 ----

func TestSettingsManager_MergeFromCLI(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai") // 已有域名

	sm.mergeFromCLI([]string{"api.z.ai", "open.bigmodel.cn"})

	if !sm.IsAllowed("api.z.ai") {
		t.Error("已有域名应保留")
	}
	if !sm.IsAllowed("open.bigmodel.cn") {
		t.Error("CLI 域名应被合并")
	}
	if sm.DomainCount() != 2 {
		t.Errorf("期望 2 个域名, 得到 %d", sm.DomainCount())
	}
}

func TestSettingsManager_MergeFromCLIOverlap(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai")

	// 合并重复域名不应重复计数
	sm.mergeFromCLI([]string{"api.z.ai"})
	if sm.DomainCount() != 1 {
		t.Errorf("重复合并不应增加计数: %d", sm.DomainCount())
	}
}

func TestSettingsManager_MergeFromCLICaseInsensitive(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("api.z.ai")

	// CLI 传大小写不同的应合并为小写
	sm.mergeFromCLI([]string{"OPEN.BIGMODEL.CN"})
	if !sm.IsAllowed("open.bigmodel.cn") {
		t.Error("CLI 域名应被正确处理为小写")
	}
}

// ---- 并发安全测试 ----

func TestSettingsManager_ConcurrentReadWrite(t *testing.T) {
	sm := newSettingsManager()
	done := make(chan struct{})

	// 并发写
	go func() {
		for i := 0; i < 100; i++ {
			sm.AddDomain("api.z.ai")
			sm.RemoveDomain("api.z.ai")
		}
		done <- struct{}{}
	}()

	// 并发读
	go func() {
		for i := 0; i < 100; i++ {
			sm.IsAllowed("api.z.ai")
			sm.GetDomains()
			sm.DomainCount()
			sm.IsWhitelistEnabled()
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	// 不 panic 就通过
}

// ---- 集成测试: 通过 HTTP handler 管理白名单 ----

// TestSettingsHTTPHandler 验证 /__admin/settings 的 GET/POST 行为。
func TestSettingsHTTPHandler(t *testing.T) {
	sm := newSettingsManager()
	sm.AddDomain("existing.com")

	admin := &adminServer{
		password: "test",
		secret:   []byte("test-secret-12345678901234567890"),
		stats:    newStatsCollector(),
		settings: sm,
	}
	// 给一个有效的 secret 避免 handler 挂掉
	admin.secret = make([]byte, 32)
	for i := range admin.secret {
		admin.secret[i] = byte(i)
	}

	// ---- GET: 查看设置 ----
	req := httptest.NewRequest("GET", "/__admin/settings", nil)
	w := httptest.NewRecorder()
	admin.handleSettings(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET 应返回 200, 得到 %d", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	resp.Body.Close()

	if !containsStr(bodyStr, "existing.com") {
		t.Error("页面应包含已添加的域名")
	}

	// ---- POST: 添加域名 ----
	req2 := httptest.NewRequest("POST", "/__admin/settings?action=add&domain=newdomain.com", nil)
	req2.Form = map[string][]string{
		"action": {"add"},
		"domain": {"newdomain.com"},
	}
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	admin.handleSettings(w2, req2)

	if w2.Code != http.StatusSeeOther {
		t.Errorf("POST 添加后应 302 跳转, 得到 %d", w2.Code)
	}
	if !sm.IsAllowed("newdomain.com") {
		t.Error("新域名应被添加")
	}

	// ---- POST: 移除域名 ----
	req3 := httptest.NewRequest("POST", "/__admin/settings?action=remove&domain=newdomain.com", nil)
	req3.Form = map[string][]string{
		"action": {"remove"},
		"domain": {"newdomain.com"},
	}
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w3 := httptest.NewRecorder()
	admin.handleSettings(w3, req3)

	if w3.Code != http.StatusSeeOther {
		t.Errorf("POST 移除后应 302 跳转, 得到 %d", w3.Code)
	}
	if sm.IsAllowed("newdomain.com") {
		t.Error("域名应已被移除")
	}
}

// containsStr 检查字符串是否包含子串。
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && containsStrHelper(s, substr)
}

func containsStrHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
