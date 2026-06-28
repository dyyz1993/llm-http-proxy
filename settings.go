// settings.go — 运行时设置管理器（域名白名单等）
//
// 管理可以通过 Web UI 修改的服务设置，支持持久化到文件。
// 当前功能:域名白名单（key 注入模式的安全控制）。
//
// 持久化文件: {persist}.settings (JSON 格式)
//
// 启动流程:
//   1. 解析 CLI -allow-domains / ALLOW_DOMAINS 环境变量
//   2. 尝试加载 {persist}.settings 文件（如果存在）
//   3. 将 CLI 域名合并进当前设置（CLI 域名作为最小安全基底）
//   4. 启动定时落盘（每 30s）

package main

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// settingsManager 管理运行时可修改的服务设置。
// 线程安全:所有方法均加锁。
type settingsManager struct {
	mu      sync.RWMutex
	domains map[string]bool // 域名白名单(小写)
	path    string          // 持久化路径
}

// settingsData 是持久化文件的 JSON 结构。
type settingsData struct {
	Domains []string `json:"domains"`
}

// newSettingsManager 创建设置管理器，初始白名单为空（不限制）。
func newSettingsManager() *settingsManager {
	return &settingsManager{
		domains: make(map[string]bool),
	}
}

// IsAllowed 检查域名是否被白名单允许。
// 白名单为空时允许所有域名（相当于"不限制"）。
func (sm *settingsManager) IsAllowed(domain string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if len(sm.domains) == 0 {
		return true // 空 = 不限制
	}
	return sm.domains[strings.ToLower(domain)]
}

// AddDomain 添加域名到白名单。空字符串或无效域名会被忽略。
func (sm *settingsManager) AddDomain(domain string) {
	d := strings.TrimSpace(strings.ToLower(domain))
	if d == "" {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.domains[d] = true
}

// RemoveDomain 从白名单移除域名。
func (sm *settingsManager) RemoveDomain(domain string) {
	d := strings.TrimSpace(strings.ToLower(domain))
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.domains, d)
}

// GetDomains 返回当前白名单域名列表（按字母序排序）。
func (sm *settingsManager) GetDomains() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]string, 0, len(sm.domains))
	for d := range sm.domains {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}

// IsWhitelistEnabled 返回白名单是否启用（非空）。
func (sm *settingsManager) IsWhitelistEnabled() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.domains) > 0
}

// DomainCount 返回白名单中的域名数量。
func (sm *settingsManager) DomainCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.domains)
}

// load 从文件加载持久化的设置。
// 文件不存在不是错误（返回 nil）。
func (sm *settingsManager) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var sd settingsData
	if err := json.Unmarshal(data, &sd); err != nil {
		return err
	}
	sm.mu.Lock()
	sm.domains = make(map[string]bool, len(sd.Domains))
	for _, d := range sd.Domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			sm.domains[d] = true
		}
	}
	sm.path = path
	sm.mu.Unlock()
	log.Printf("已从 %s 加载设置: %d 个域名", path, len(sd.Domains))
	return nil
}

// save 将当前设置持久化到文件（原子写入:临时文件 + rename）。
func (sm *settingsManager) save(path string) error {
	sm.mu.RLock()
	domains := make([]string, 0, len(sm.domains))
	for d := range sm.domains {
		domains = append(domains, d)
	}
	hasPath := sm.path
	sm.mu.RUnlock()

	// 如果没有域名，直接删文件（空＝不限制，无需持久化）
	if len(domains) == 0 && path != "" {
		os.Remove(path) // 忽略错误
		os.Remove(path + ".tmp")
		return nil
	}

	sort.Strings(domains)
	sd := settingsData{Domains: domains}
	data, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return err
	}
	// 原子写入:先写临时文件再 rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // 清理临时文件
		return err
	}
	if hasPath == "" {
		sm.mu.Lock()
		sm.path = path
		sm.mu.Unlock()
	}
	return nil
}

// startPersistLoop 启动定时落盘 goroutine。
// 每 interval 将当前设置写入 path。
func (sm *settingsManager) startPersistLoop(path string, interval time.Duration) {
	sm.mu.Lock()
	sm.path = path
	sm.mu.Unlock()
	go func() {
		for {
			time.Sleep(interval)
			sm.mu.RLock()
			p := sm.path
			sm.mu.RUnlock()
			if p == "" {
				continue
			}
			if err := sm.save(p); err != nil {
				log.Printf("持久化设置失败: %v", err)
			}
		}
	}()
}

// mergeFromCLI 将 CLI/env 传入的初始域名合并进当前白名单。
// CLI 域名始终存在（作为最小安全基底），重启后也会重新合并。
// 如果已有持久化的设置，CLI 域名不会覆盖已有值，只会补充。
func (sm *settingsManager) mergeFromCLI(cliDomains []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	added := 0
	for _, d := range cliDomains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" && !sm.domains[d] {
			sm.domains[d] = true
			added++
		}
	}
	if added > 0 {
		log.Printf("从 CLI/env 合并了 %d 个域名到白名单", added)
	}
}

// persistSettingsPath 返回设置持久化文件路径。
// 基于主持久化路径,附加 .settings 后缀。
// persist 为空时返回空字符串(不持久化)。
func persistSettingsPath(persist string) string {
	if persist == "" {
		return ""
	}
	return persist + ".settings"
}
