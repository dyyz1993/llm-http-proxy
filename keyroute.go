// keyroute.go — key 注入模式 + 按路径限流
//
// 用户通过 /k/{alias}/https://目标 访问,代理从服务端配置(keys.yaml)查 alias,
// 自动注入对应的 API key 到请求头。用户永远看不到真实 key。
//
// 配置示例见 keys.example.yaml。配置文件支持热加载(每 10s 重读 mtime)。

package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// KeyConfig 是单个 alias 的注入规则 + 可选限流 + 可选有效期 + 可选用量限额 + 可选禁止时段。
type KeyConfig struct {
	Key     string `yaml:"key"`     // 真实 API key(不对外暴露)
	Header  string `yaml:"header"`  // 注入到哪个 header: Authorization / x-api-key / api-key
	Prefix  string `yaml:"prefix"`  // 可选前缀(如 Authorization 需要 "Bearer ")
	Rate    int    `yaml:"rate"`    // 限流:每分钟补充令牌数(0=不限流)
	Burst   int    `yaml:"burst"`   // 限流:桶容量上限(突发上限)
	Expires string `yaml:"expires"` // 可选有效期:"YYYY-MM-DD"(到当天结束) 或 "YYYY-MM-DD HH:MM"(精确到分,北京时间)。空=永久
	// 用量限额(窗口内,0=不限)
	MaxTokens int64  `yaml:"max_tokens"`   // 窗口内总 token 上限(输入+输出),0=不限
	MaxReqs   int64  `yaml:"max_requests"` // 窗口内成功请求次数上限(HTTP 2xx/3xx),0=不限
	Window    string `yaml:"window"`       // 窗口时长,如 "5h"/"24h"/"7d"/"30d"。空=默认 100d(硬上限)
	// 禁止时段(北京时间,每天重复)
	TimeBlock *TimeBlock `yaml:"time_block"` // 可选:在此时段内请求返回 403
}

// HasQuota 返回该 alias 是否配置了用量限额。
func (cfg KeyConfig) HasQuota() bool {
	return cfg.MaxTokens > 0 || cfg.MaxReqs > 0
}

// WindowDuration 解析窗口时长。空或解析失败返回默认 100 天。
func (cfg KeyConfig) WindowDuration() time.Duration {
	if cfg.Window == "" {
		return 100 * 24 * time.Hour
	}
	d, err := parseWindowDuration(cfg.Window)
	if err != nil {
		return 100 * 24 * time.Hour // 解析失败用默认
	}
	return d
}

// parseWindowDuration 解析窗口时长字符串,支持 "h"(小时) 和 "d"(天)。
func parseWindowDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty window duration")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid window duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// TimeBlock 定义每天重复的禁止访问时段(北京时间)。
// Start/End 格式为 "HH:MM",如 "22:00"、"08:00"。
//   - start < end: 区间内禁止(如 09:00-18:00)
//   - start > end: 跨午夜禁止(如 22:00-08:00, 22:00~00:00 + 00:00~08:00)
//   - start == end: 全天禁止
//
// nil/不配置 = 不禁止。
type TimeBlock struct {
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

// IsBlocked 检查当前时间(北京时间)是否在禁止时段内。
func (tb *TimeBlock) IsBlocked(now time.Time) bool {
	if tb == nil || tb.Start == "" || tb.End == "" {
		return false
	}
	// 转北京时间
	now = now.In(beijing)
	sh, sm, ok1 := parseHHMM(tb.Start)
	eh, em, ok2 := parseHHMM(tb.End)
	if !ok1 || !ok2 {
		return false
	}
	startMin := sh*60 + sm
	endMin := eh*60 + em
	curMin := now.Hour()*60 + now.Minute()

	if startMin < endMin {
		// 单日区间: [start, end)
		return curMin >= startMin && curMin < endMin
	}
	if startMin > endMin {
		// 跨日区间: [start, 00:00) ∪ [00:00, end)
		return curMin >= startMin || curMin < endMin
	}
	// startMin == endMin: 全天禁止
	return true
}

// parseHHMM 把 "HH:MM" 解析成小时、分钟。
func parseHHMM(s string) (hour, min int, ok bool) {
	_, err := fmt.Sscanf(s, "%d:%d", &hour, &min)
	if err != nil {
		return 0, 0, false
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return 0, 0, false
	}
	return hour, min, true
}

// expiresLayouts 是支持的有效期格式,按优先级尝试。
// 全部按北京时间解析(用户填的就是北京时间)。
var expiresLayouts = []string{
	"2006-01-02 15:04",    // 时分(精确到分)
	"2006-01-02 15:04:05", // 时分秒(也兼容)
	"2006-01-02",          // 纯日期(到当天结束,兼容老配置)
}

// parseExpires 把 expires 字符串解析成北京时间的时间点。
// 纯日期格式("2026-06-22") → 当天结束(23:59:59),即"这一天内都有效"。
// 时分格式("2026-06-22 09:00") → 精确到那个时刻。
// 注意:同时接受 datetime-local 控件提交的 ISO 格式("2026-06-22T09:00")。
// 解析失败返回零值 + false。
func parseExpires(s string) (time.Time, bool) {
	// datetime-local 控件提交的是 ISO 格式(用 T 分隔),统一换成空格再解析
	s = strings.Replace(strings.TrimSpace(s), "T", " ", 1)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range expiresLayouts {
		// 用 ParseInLocation 保证按北京时间解释(而不是 UTC)
		if t, err := time.ParseInLocation(layout, s, beijing); err == nil {
			// 纯日期格式:补到当天结束(23:59:59),保证当天内仍可用
			if layout == "2006-01-02" {
				return t.Add(23*time.Hour + 59*time.Minute + 59*time.Second), true
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// normalizeExpires 把有效期规范化成统一的存储格式(空格分隔)。
// datetime-local 提交 "2026-06-22T09:00" → 存为 "2026-06-22 09:00"。
// 非法/空字符串原样返回(由调用方决定是否接受)。
func normalizeExpires(s string) string {
	s = strings.TrimSpace(s)
	return strings.Replace(s, "T", " ", 1)
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
	mu               sync.RWMutex
	configs          map[string]KeyConfig // alias → config
	limiters         map[string]*rateLimiter
	path             string
	lastMtime        time.Time
	imageFilter      []ImageFilterRule     // 全局 image_url 过滤规则
	tokenMultipliers []TokenMultiplierRule // 全局 Token 用量乘数规则
}

// newKeyStore 创建空的 key store。
func newKeyStore() *keyStore {
	return &keyStore{
		configs:  make(map[string]KeyConfig),
		limiters: make(map[string]*rateLimiter),
	}
}

// load 从 path 读取 keys.yaml。文件不存在视为空配置(不报错)。
// 支持在 keys.yaml 中同时存放 alias 配置和全局 image_filter 规则（向下兼容）。
func (ks *keyStore) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动,没文件很正常
		}
		return err
	}

	// 两步解析：先提取 image_filter 和 token_multipliers，再解析剩余部分为 keys。
	// 这样 keys.yaml 现有格式不变，只需加顶层字段。
	var rules []ImageFilterRule
	var multipliers []TokenMultiplierRule
	var configs map[string]KeyConfig
	{
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return err
		}
		if filterRaw, ok := raw["image_filter"]; ok {
			filterData, _ := yaml.Marshal(filterRaw)
			if err := yaml.Unmarshal(filterData, &rules); err != nil {
				log.Printf("解析 image_filter 配置失败: %v", err)
			}
		}
		if multRaw, ok := raw["token_multipliers"]; ok {
			multData, _ := yaml.Marshal(multRaw)
			if err := yaml.Unmarshal(multData, &multipliers); err != nil {
				log.Printf("解析 token_multipliers 配置失败: %v", err)
			}
		}
		// 去掉已知的全局字段，剩余部分作为 keys 解析
		delete(raw, "image_filter")
		delete(raw, "token_multipliers")
		keysData, _ := yaml.Marshal(raw)
		if err := yaml.Unmarshal(keysData, &configs); err != nil {
			return err
		}
	}

	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.configs = configs
	ks.imageFilter = rules
	ks.tokenMultipliers = multipliers
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
	log.Printf("已加载 %d 个 key 配置, %d 条 image_filter 规则, %d 条 token_multipliers 规则",
		len(configs), len(rules), len(multipliers))
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

// lookup 查找某个 alias 的配置。不存在或已过期返回 ok=false。
func (ks *keyStore) lookup(alias string) (KeyConfig, bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	cfg, ok := ks.configs[alias]
	if !ok {
		return cfg, false
	}
	// 检查有效期(parseExpires 统一处理时分/纯日期 + 北京时区)
	if cfg.Expires != "" {
		if exp, ok := parseExpires(cfg.Expires); ok && time.Now().After(exp) {
			return cfg, false // 已过期
		}
	}
	return cfg, true
}

// isExpired 检查 alias 是否存在但已过期(用于区分"不存在"和"过期")。
func (ks *keyStore) isExpired(alias string) bool {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	cfg, ok := ks.configs[alias]
	if !ok || cfg.Expires == "" {
		return false
	}
	exp, ok := parseExpires(cfg.Expires)
	if !ok {
		return false
	}
	return time.Now().After(exp)
}

// expiredMap 返回所有 alias 的过期状态(给管理界面置灰用)。
// 只在渲染 Keys 页时调用一次,不影响请求路径性能。
func (ks *keyStore) expiredMap() map[string]bool {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	now := time.Now()
	out := make(map[string]bool, len(ks.configs))
	for alias, cfg := range ks.configs {
		if cfg.Expires == "" {
			continue
		}
		exp, ok := parseExpires(cfg.Expires)
		if ok && now.After(exp) {
			out[alias] = true
		}
	}
	return out
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

// getImageFilter 返回 image_filter 规则快照。
// 调用方得到的是规则切片的副本（切片头复制但底层数组共享），
// 但热加载时整个切片会被替换，读取方不受影响。
func (ks *keyStore) getImageFilter() []ImageFilterRule {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.imageFilter
}

// getTokenMultipliers 返回 token_multipliers 规则快照。
func (ks *keyStore) getTokenMultipliers() []TokenMultiplierRule {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.tokenMultipliers
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
