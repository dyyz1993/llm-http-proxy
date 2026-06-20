// keyroute.go — key 注入模式 + 按路径限流
//
// 用户通过 /k/{alias}/https://目标 访问,代理从服务端配置(keys.yaml)查 alias,
// 自动注入对应的 API key 到请求头。用户永远看不到真实 key。
//
// 配置示例见 keys.example.yaml。配置文件支持热加载(每 10s 重读 mtime)。

package main

import (
	"log"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// KeyConfig 是单个 alias 的注入规则 + 可选限流。
type KeyConfig struct {
	Key    string `yaml:"key"`    // 真实 API key(不对外暴露)
	Header string `yaml:"header"` // 注入到哪个 header: Authorization / x-api-key / api-key
	Prefix string `yaml:"prefix"` // 可选前缀(如 Authorization 需要 "Bearer ")
	Rate   int    `yaml:"rate"`   // 限流:每分钟补充令牌数(0=不限流)
	Burst  int    `yaml:"burst"`  // 限流:桶容量上限(突发上限)
}

// rateLimiter 是单个别名的令牌桶状态。
type rateLimiter struct {
	mu         sync.Mutex
	tokens     float64 // 当前令牌数
	lastFill   time.Time
	ratePerSec float64 // 每秒补充(由 Rate/60 算出)
	burst      float64
}

// keyStore 管理 keys.yaml 的加载、热重载、限流器。
type keyStore struct {
	mu        sync.RWMutex
	configs   map[string]KeyConfig // alias → config
	limiters  map[string]*rateLimiter
	path      string
	lastMtime time.Time
}

// newKeyStore 创建空的 key store。
func newKeyStore() *keyStore {
	return &keyStore{
		configs:  make(map[string]KeyConfig),
		limiters: make(map[string]*rateLimiter),
	}
}

// load 从 path 读取 keys.yaml。文件不存在视为空配置(不报错)。
func (ks *keyStore) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动,没文件很正常
		}
		return err
	}
	var configs map[string]KeyConfig
	if err := yaml.Unmarshal(data, &configs); err != nil {
		return err
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.configs = configs
	ks.path = path
	if fi, err := os.Stat(path); err == nil {
		ks.lastMtime = fi.ModTime()
	}
	// 为有限流的 alias 创建/更新限流器
	for alias, cfg := range configs {
		if cfg.Rate > 0 {
			ks.getOrCreateLimiter(alias, cfg)
		}
	}
	log.Printf("已加载 %d 个 key 配置", len(configs))
	return nil
}

// getOrCreateLimiter 获取或创建某个 alias 的限流器。
// 调用方需持 ks.mu 写锁。
func (ks *keyStore) getOrCreateLimiter(alias string, cfg KeyConfig) *rateLimiter {
	rl, ok := ks.limiters[alias]
	ratePerSec := float64(cfg.Rate) / 60.0
	burst := float64(cfg.Burst)
	if burst == 0 {
		burst = ratePerSec // 默认 burst = 1 秒的额度
	}
	if !ok {
		// 新建:令牌桶初始满
		rl = &rateLimiter{
			tokens:     burst,
			lastFill:   time.Now(),
			ratePerSec: ratePerSec,
			burst:      burst,
		}
		ks.limiters[alias] = rl
	} else {
		// 更新参数(热加载后 rate/burst 可能变了)
		rl.mu.Lock()
		rl.ratePerSec = ratePerSec
		rl.burst = burst
		rl.mu.Unlock()
	}
	return rl
}

// reloadIfChanged 检查文件 mtime,变了才重读。
func (ks *keyStore) reloadIfChanged() {
	if ks.path == "" {
		return
	}
	fi, err := os.Stat(ks.path)
	if err != nil {
		return
	}
	if !fi.ModTime().After(ks.lastMtime) {
		return // 没变
	}
	log.Printf("检测到 %s 变化,重新加载...", ks.path)
	if err := ks.load(ks.path); err != nil {
		log.Printf("重载 key 配置失败(保留旧配置): %v", err)
	}
}

// startReloadLoop 每 10 秒检查一次配置文件是否变化。
func (ks *keyStore) startReloadLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			ks.reloadIfChanged()
		}
	}()
}

// lookup 查找某个 alias 的配置。不存在返回 ok=false。
func (ks *keyStore) lookup(alias string) (KeyConfig, bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	cfg, ok := ks.configs[alias]
	return cfg, ok
}

// allow 检查 alias 是否被限流放行。返回 true=允许,false=超限。
func (ks *keyStore) allow(alias string) bool {
	ks.mu.RLock()
	cfg, ok := ks.configs[alias]
	rl := ks.limiters[alias]
	ks.mu.RUnlock()
	if !ok || cfg.Rate <= 0 {
		return true // 无限流
	}
	if rl == nil {
		return true // 限流器未创建(防御)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// 补充令牌(按距上次的时间差)
	now := time.Now()
	elapsed := now.Sub(rl.lastFill).Seconds()
	rl.tokens += elapsed * rl.ratePerSec
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.lastFill = now
	if rl.tokens >= 1.0 {
		rl.tokens -= 1.0
		return true
	}
	return false
}

// --- 管理用写方法(给 admin UI 用) ---

// allConfigs 返回所有 alias 配置的快照(深拷贝)。
func (ks *keyStore) allConfigs() map[string]KeyConfig {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	out := make(map[string]KeyConfig, len(ks.configs))
	for k, v := range ks.configs {
		out[k] = v
	}
	return out
}

// save 写入 ks.path(原子写:tmp+rename)。调用方需持写锁。
func (ks *keyStore) saveLocked() error {
	if ks.path == "" {
		return nil
	}
	data, err := yaml.Marshal(ks.configs)
	if err != nil {
		return err
	}
	tmp := ks.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, ks.path); err != nil {
		return err
	}
	// 更新 mtime,避免 reloadIfChanged 立刻重读
	if fi, err := os.Stat(ks.path); err == nil {
		ks.lastMtime = fi.ModTime()
	}
	return nil
}

// setConfig 新增/更新一个 alias 配置,并持久化到文件。
func (ks *keyStore) setConfig(alias string, cfg KeyConfig) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.configs[alias] = cfg
	if cfg.Rate > 0 {
		ks.getOrCreateLimiter(alias, cfg)
	}
	return ks.saveLocked()
}

// deleteConfig 删除一个 alias 配置,并持久化。
func (ks *keyStore) deleteConfig(alias string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	delete(ks.configs, alias)
	delete(ks.limiters, alias)
	return ks.saveLocked()
}
