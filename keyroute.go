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
//
// 所有拦截器参数可以写在 alias 里直接使用,也可以通过 profile 引用模板后再用 override
// 局部覆盖。详见 resolveConfig。
type KeyConfig struct {
	Key     string `yaml:"key"`     // 真实 API key(不对外暴露)
	Header  string `yaml:"header"`  // 注入到哪个 header: Authorization / x-api-key / api-key
	Prefix  string `yaml:"prefix"`  // 可选前缀(如 Authorization 需要 "Bearer ")
	Profile string `yaml:"profile"` // 引用拦截器模板 ID(interceptor_profiles 段)
	// 拦截器参数(可以直接写在这里,或通过 profile 引用+override 覆盖)
	Rate      int                 `yaml:"rate"`         // 限流:每分钟补充令牌数(0=不限流)
	Burst     int                 `yaml:"burst"`        // 限流:桶容量上限(突发上限)
	Expires   string              `yaml:"expires"`      // 可选有效期
	MaxTokens int64               `yaml:"max_tokens"`   // 窗口内总 token 上限
	MaxReqs   int64               `yaml:"max_requests"` // 窗口内成功请求次数上限
	Window    string              `yaml:"window"`       // 窗口时长
	TimeBlock *TimeBlock          `yaml:"time_block"`   // 禁止时段
	Override  *InterceptorProfile `yaml:"override"`     // 覆盖 profile 中的部分参数
}

// InterceptorProfile 是拦截器模板,在 interceptor_profiles 段定义,被 KeyConfig.Profile 引用。
type InterceptorProfile struct {
	Rate      int        `yaml:"rate"`
	Burst     int        `yaml:"burst"`
	Expires   string     `yaml:"expires"`
	MaxTokens int64      `yaml:"max_tokens"`
	MaxReqs   int64      `yaml:"max_requests"`
	Window    string     `yaml:"window"`
	TimeBlock *TimeBlock `yaml:"time_block"`
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

// resolveConfig 将 interceptor_profiles 模板与 KeyConfig 合并。
//
// 合并优先级(高→低):
//
//  1. KeyConfig 直接字段(alias 里直接写的)
//  2. KeyConfig.Override 字段
//  3. KeyConfig.Profile 引用的模板
//  4. defaultProfile(如果 Profile 为空且存在该模板)
//
// 没有 profile 且没有 defaultProfile → 原样返回。
func resolveConfig(cfg KeyConfig, profiles map[string]InterceptorProfile, defaultProfile string) KeyConfig {
	// 确定 base profile
	profileID := cfg.Profile
	if profileID == "" {
		profileID = defaultProfile
	}
	if profileID == "" || profiles == nil {
		return cfg
	}
	base, ok := profiles[profileID]
	if !ok {
		return cfg // 找不到 profile → 原样
	}

	// 合并优先级(高→低): alias 直接字段 > override > profile
	// 先保存 alias 直接字段
	direct := struct {
		rate, burst        int
		expires            string
		maxTokens, maxReqs int64
		window             string
		timeBlock          *TimeBlock
	}{cfg.Rate, cfg.Burst, cfg.Expires, cfg.MaxTokens, cfg.MaxReqs, cfg.Window, cfg.TimeBlock}

	// 从 profile 开始
	result := KeyConfig{
		Key:    cfg.Key,
		Header: cfg.Header,
		Prefix: cfg.Prefix,
	}
	mergeInto(&result, base)

	// override 覆盖
	if cfg.Override != nil {
		mergeInto(&result, *cfg.Override)
	}

	// alias 直接字段(最高优先级)
	mergeInto(&result, InterceptorProfile{
		Rate:      direct.rate,
		Burst:     direct.burst,
		Expires:   direct.expires,
		MaxTokens: direct.maxTokens,
		MaxReqs:   direct.maxReqs,
		Window:    direct.window,
		TimeBlock: direct.timeBlock,
	})

	return result
}

// mergeInto 将 src 的非零非空字段合并到 target。
func mergeInto(target *KeyConfig, src InterceptorProfile) {
	if src.Rate != 0 {
		target.Rate = src.Rate
	}
	if src.Burst != 0 {
		target.Burst = src.Burst
	}
	if src.Expires != "" {
		target.Expires = src.Expires
	}
	if src.MaxTokens != 0 {
		target.MaxTokens = src.MaxTokens
	}
	if src.MaxReqs != 0 {
		target.MaxReqs = src.MaxReqs
	}
	if src.Window != "" {
		target.Window = src.Window
	}
	if src.TimeBlock != nil {
		target.TimeBlock = src.TimeBlock
	}
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
	mu                  sync.RWMutex
	configs             map[string]KeyConfig // alias → config
	limiters            map[string]*rateLimiter
	path                string
	lastMtime           time.Time
	imageFilter         []ImageFilterRule             // 全局 image_url 过滤规则
	tokenMultipliers    []TokenMultiplierRule         // 全局 Token 用量乘数规则
	interceptorProfiles map[string]InterceptorProfile // 拦截器模板(interceptor_profiles 段)
	retryConfig         RetryConfig                   // 上游请求重试配置
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
	var profiles map[string]InterceptorProfile
	var retryCfg RetryConfig
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
		if profRaw, ok := raw["interceptor_profiles"]; ok {
			profData, _ := yaml.Marshal(profRaw)
			if err := yaml.Unmarshal(profData, &profiles); err != nil {
				log.Printf("解析 interceptor_profiles 配置失败: %v", err)
			}
		}
		if retryRaw, ok := raw["retry"]; ok {
			retryData, _ := yaml.Marshal(retryRaw)
			if err := yaml.Unmarshal(retryData, &retryCfg); err != nil {
				log.Printf("解析 retry 配置失败: %v", err)
			}
		}
		// 去掉已知的全局字段，剩余部分作为 keys 解析
		delete(raw, "image_filter")
		delete(raw, "token_multipliers")
		delete(raw, "interceptor_profiles")
		delete(raw, "retry")
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
	ks.interceptorProfiles = profiles
	ks.retryConfig = retryCfg
	ks.path = path
	if fi, err := os.Stat(path); err == nil {
		ks.lastMtime = fi.ModTime()
	}
	// 为有限流的 alias 创建/更新限流器(用合并模板后的配置)
	for alias, cfg := range configs {
		resolved := resolveConfig(cfg, profiles, "default")
		if resolved.Rate > 0 {
			ks.getOrCreateLimiter(alias, resolved)
		}
	}
	log.Printf("已加载 %d 个 key 配置, %d 条 image_filter 规则, %d 条 token_multipliers 规则, %d 个拦截器模板",
		len(configs), len(rules), len(multipliers), len(profiles))
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

// getPath 返回 keys.yaml 路径(线程安全)。
func (ks *keyStore) getPath() string {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.path
}

// forceReload 强制重新加载 keys.yaml(给 admin UI 保存后调用)。
func (ks *keyStore) forceReload() {
	path := func() string {
		ks.mu.RLock()
		defer ks.mu.RUnlock()
		return ks.path
	}()
	if path == "" {
		return
	}
	log.Printf("强制重新加载 %s ...", path)
	if err := ks.load(path); err != nil {
		log.Printf("强制重载 key 配置失败(保留旧配置): %v", err)
	}
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

// getInterceptorProfiles 返回 interceptor_profiles 模板副本(只读)。
func (ks *keyStore) getInterceptorProfiles() map[string]InterceptorProfile {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	if ks.interceptorProfiles == nil {
		return nil
	}
	cp := make(map[string]InterceptorProfile, len(ks.interceptorProfiles))
	for k, v := range ks.interceptorProfiles {
		cp[k] = v
	}
	return cp
}

// getRetryConfig 返回上游重试配置副本。
func (ks *keyStore) getRetryConfig() RetryConfig {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.retryConfig
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

// saveYAMLAll 将完整 YAML 写入文件(原子写:tmp+rename)。
// 会合并内存中的 alias 配置 + interceptor profiles + 全局规则。
// 写入前用 validateYAML 做结构校验,校验不通过不写。
// 调用方需持写锁。
func (ks *keyStore) saveYAMLAll() error {
	if ks.path == "" {
		return nil
	}
	// 读取当前原始 YAML(保留 comment 不现实,至少保留全局段结构)
	raw, err := os.ReadFile(ks.path)
	var full map[string]interface{}
	if err == nil {
		// 能读就解析,不能读就新建
		yaml.Unmarshal(raw, &full)
	}
	if full == nil {
		full = make(map[string]interface{})
	}

	// 替换 interceptor_profiles 段
	full["interceptor_profiles"] = ks.interceptorProfiles

	// 替换 alias 配置段(先把 alias 从 full 里去重,再填充 configs)
	for k := range full {
		// 跳过已知全局段
		if k == "image_filter" || k == "token_multipliers" || k == "interceptor_profiles" {
			continue
		}
		// 如果不是 configs 里的 alias,保留;否则让 configs 覆盖
		if _, ok := ks.configs[k]; !ok {
			continue
		}
		// 是 alias 且已在 full 中,删掉稍后由 configs 统一写入
		delete(full, k)
	}
	for k, v := range ks.configs {
		full[k] = v
	}

	data, err := yaml.Marshal(full)
	if err != nil {
		return err
	}
	// 结构校验(和 /__admin/config POST 一样严格)
	if err := validateYAML(data); err != nil {
		return fmt.Errorf("保存前校验失败: %w", err)
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
	return ks.saveYAMLAll()
}

// deleteConfig 删除一个 alias 配置,并持久化。
func (ks *keyStore) deleteConfig(alias string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	delete(ks.configs, alias)
	delete(ks.limiters, alias)
	return ks.saveYAMLAll()
}

// setProfile 新增/更新一个拦截器模板,并持久化。
func (ks *keyStore) setProfile(name string, profile InterceptorProfile) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.interceptorProfiles == nil {
		ks.interceptorProfiles = make(map[string]InterceptorProfile)
	}
	ks.interceptorProfiles[name] = profile
	return ks.saveYAMLAll()
}

// deleteProfile 删除一个拦截器模板,并持久化。
func (ks *keyStore) deleteProfile(name string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	delete(ks.interceptorProfiles, name)
	return ks.saveYAMLAll()
}

// allProfiles 返回所有拦截器模板的快照(深拷贝)。
func (ks *keyStore) allProfiles() map[string]InterceptorProfile {
	return ks.getInterceptorProfiles()
}
