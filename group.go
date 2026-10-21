// group.go — 别名池(Group)功能
//
// Group 是多个 alias 的"池子",按优先级依次尝试。
// 访问 /k/{group}/... 时,挑第一个可用的成员转发;
// 成员失败(402/429/502 等)则标记冷却,自动换下一个成员重试。
// 客户端完全无感知。
//
// 配置在 keys.yaml 的 groups 段:
//
//	groups:
//	  glm-pool:
//	    members: [glm, max-0, channel]  # 按顺序试
//	    on_status: [402, 429, 502, 503] # 这些码触发换人
//	    cooldown: 5m                    # 失败成员冷却时间

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// GroupConfig 是单个 group 的配置。
type GroupConfig struct {
	Members   []string           `yaml:"members"`             // 成员 alias 列表(实际顺序由动态排序决定,见 sortGroupMembersDynamic)
	Pinned    []string           `yaml:"pinned,omitempty"`    // 置顶成员:不在冷却期时最优先使用(压过动态排序所有档位),冷却期间自动回落到其他成员
	PinRules  map[string]PinRule `yaml:"pin_rules,omitempty"` // alias → 置顶自动解除条件(无规则=永久置顶)
	OnStatus  []int              `yaml:"on_status"`           // 上游返回这些码时触发换人
	Cooldown  string             `yaml:"cooldown"`            // 冷却时间(如 "5m", "30s")
	Expires   string             `yaml:"expires"`             // 可选有效期(空=永久)
	MaxTokens int64              `yaml:"max_tokens"`          // 窗口内总 token 上限(0=不限)
	MaxReqs   int64              `yaml:"max_requests"`        // 窗口内成功请求次数上限(0=不限)
	Window    string             `yaml:"window"`              // 窗口时长(如 "24h", "7d")
	Disabled  bool               `yaml:"disabled"`            // 禁用开关(禁用时返回 503)
}

// PinRule 给置顶加自动解除条件(读取时判定,见 activePins)。
// Until 是北京时间绝对时间("2006-01-02 15:04");MaxReqs 是置顶期间该成员
// 成功服务的请求数上限(429/失败不计,BaseReqs 是置顶时的成功数基线)。
// 过期规则留在配置里是惰性的(不影响行为),下次置顶/取消/编辑群组时自然清理。
type PinRule struct {
	Until    string `yaml:"until,omitempty"`         // 自动取消的绝对时间(空=不限时)
	MaxReqs  int64  `yaml:"max_requests,omitempty"`  // 置顶期间成功请求数上限(0=不限)
	BaseReqs int64  `yaml:"base_requests,omitempty"` // 置顶时的成功请求数基线
}

// memberState 记录单个成员的实时状态(内存,不持久化)。
type memberState struct {
	calias       string        // 成员 alias
	coolUntil    time.Time     // 冷却截止时间(零值=未冷却)
	lastStatus   int           // 最后一次上游返回的状态码
	lastFail     time.Time     // 最后一次失败时间
	failCount    int           // 连续失败次数
	statusCounts map[int]int64 // 各状态码累计计数(只统计 group 路由)
	totalReqs    int64         // group 路由总请求数(含成功和失败)
}

// memberStatusSnapshot 是给 UI 展示用的只读快照。
type memberStatusSnapshot struct {
	Alias        string
	CoolUntil    time.Time
	IsCooling    bool
	LastStatus   int
	LastFail     time.Time
	FailCount    int
	TotalReqs    int64
	StatusCounts map[int]int64
}

// groupStatusSnapshot 是整个 group 的状态快照。
type groupStatusSnapshot struct {
	Name    string
	Config  GroupConfig
	Members []memberStatusSnapshot
}

// groupManager 管理所有 group 的配置和成员冷却状态。
type groupManager struct {
	mu             sync.RWMutex
	groups         map[string]GroupConfig  // group 名 → 配置
	states         map[string]*memberState // alias → 状态(所有 group 共享一份)
	requestCounter int64                   // 全局请求计数器,用于轮询偏移
}

// nextOffset 返回当前偏移值并递增计数器。
func (gm *groupManager) nextOffset() int64 {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.requestCounter++
	return gm.requestCounter
}

// newGroupManager 创建空的 group 管理器。
func newGroupManager() *groupManager {
	return &groupManager{
		groups: make(map[string]GroupConfig),
		states: make(map[string]*memberState),
	}
}

// updateGroups 更新 group 配置(热加载时调用)。
// 保留已有成员的冷却状态,只为新成员创建状态。
func (gm *groupManager) updateGroups(groups map[string]GroupConfig) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	gm.groups = make(map[string]GroupConfig, len(groups))
	for name, cfg := range groups {
		gm.groups[name] = cfg
		// 为每个成员确保有状态条目
		for _, member := range cfg.Members {
			if _, ok := gm.states[member]; !ok {
				gm.states[member] = &memberState{calias: member}
			}
		}
	}
}

// isGroup 检查 name 是否是一个已配置的 group。
func (gm *groupManager) isGroup(name string) bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	_, ok := gm.groups[name]
	return ok
}

// pickMember 按优先级返回第一个未冷却的成员。
// 所有成员都冷却了返回 ""。
// allCooling 返回是否全部成员都在冷却中。
func (gm *groupManager) pickMember(groupName string) (member string, allCooling bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	cfg, ok := gm.groups[groupName]
	if !ok {
		return "", false
	}

	now := time.Now()
	for _, m := range cfg.Members {
		st := gm.states[m]
		if st == nil || now.After(st.coolUntil) {
			return m, false
		}
	}
	return "", true
}

// shouldSwitchStatus 检查给定的 HTTP 状态码是否应该触发换人。
func (gm *groupManager) shouldSwitchStatus(groupName string, statusCode int) bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	cfg, ok := gm.groups[groupName]
	if !ok {
		return false
	}
	for _, code := range cfg.OnStatus {
		if code == statusCode {
			return true
		}
	}
	return false
}

// markCooldown 标记成员冷却,并记录状态码。
func (gm *groupManager) markCooldown(alias string, groupName string, statusCode int) {
	gm.markCooldownWithDuration(alias, groupName, statusCode, 0)
}

// markCooldownWithDuration 标记成员冷却(自定义时长,0=用 group 配置)。
func (gm *groupManager) markCooldownWithDuration(alias string, groupName string, statusCode int, duration time.Duration) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	cooldown := duration
	if cooldown <= 0 {
		cooldown = gm.parseCooldown(groupName)
	}

	st, ok := gm.states[alias]
	if !ok {
		st = &memberState{calias: alias, statusCounts: make(map[int]int64)}
		gm.states[alias] = st
	}
	if st.statusCounts == nil {
		st.statusCounts = make(map[int]int64)
	}
	st.coolUntil = time.Now().Add(cooldown)
	st.lastStatus = statusCode
	st.lastFail = time.Now()
	st.failCount++
	st.totalReqs++
	st.statusCounts[statusCode]++
}

// markSuccess 标记成员成功(清除冷却和失败计数,记录 2xx)。
func (gm *groupManager) markSuccess(alias string, statusCode int) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	st, ok := gm.states[alias]
	if !ok {
		st = &memberState{calias: alias, statusCounts: make(map[int]int64)}
		gm.states[alias] = st
	}
	if st.statusCounts == nil {
		st.statusCounts = make(map[int]int64)
	}
	st.coolUntil = time.Time{}
	st.failCount = 0
	st.lastStatus = statusCode
	st.totalReqs++
	st.statusCounts[statusCode]++
}

// parseCooldown 解析 group 的冷却时长(内部方法,调用方持锁)。
func (gm *groupManager) parseCooldown(groupName string) time.Duration {
	cfg, ok := gm.groups[groupName]
	if !ok || cfg.Cooldown == "" {
		return 5 * time.Minute // 默认 5 分钟
	}
	d, err := parseWindowDuration(cfg.Cooldown)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

// snapshotFromState 从 memberState 构建快照(内部方法,调用方持读锁)。
func snapshotFromState(alias string, st *memberState) memberStatusSnapshot {
	now := time.Now()
	snap := memberStatusSnapshot{
		Alias:      alias,
		CoolUntil:  st.coolUntil,
		IsCooling:  st.coolUntil.After(now),
		LastStatus: st.lastStatus,
		LastFail:   st.lastFail,
		FailCount:  st.failCount,
		TotalReqs:  st.totalReqs,
	}
	if st.statusCounts != nil {
		snap.StatusCounts = make(map[int]int64, len(st.statusCounts))
		for k, v := range st.statusCounts {
			snap.StatusCounts[k] = v
		}
	}
	return snap
}

// memberStatus 返回成员的当前状态快照。
func (gm *groupManager) memberStatus(alias string) memberStatusSnapshot {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	st, ok := gm.states[alias]
	if !ok {
		return memberStatusSnapshot{Alias: alias}
	}
	return snapshotFromState(alias, st)
}

// groupStatus 返回整个 group 的状态快照。
func (gm *groupManager) groupStatus(groupName string) groupStatusSnapshot {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	cfg, ok := gm.groups[groupName]
	if !ok {
		return groupStatusSnapshot{Name: groupName}
	}

	members := make([]memberStatusSnapshot, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		st, exists := gm.states[m]
		if !exists {
			members = append(members, memberStatusSnapshot{Alias: m})
			continue
		}
		members = append(members, snapshotFromState(m, st))
	}

	return groupStatusSnapshot{
		Name:    groupName,
		Config:  cfg,
		Members: members,
	}
}

// allGroupStatus 返回所有 group 的状态快照。
func (gm *groupManager) allGroupStatus() []groupStatusSnapshot {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	result := make([]groupStatusSnapshot, 0, len(gm.groups))
	for name, cfg := range gm.groups {
		members := make([]memberStatusSnapshot, 0, len(cfg.Members))
		for _, m := range cfg.Members {
			st, exists := gm.states[m]
			if !exists {
				members = append(members, memberStatusSnapshot{Alias: m})
				continue
			}
			members = append(members, snapshotFromState(m, st))
		}
		result = append(result, groupStatusSnapshot{
			Name:    name,
			Config:  cfg,
			Members: members,
		})
	}
	return result
}

// reorderPinnedFirst 把置顶成员按原列表相对顺序提到最前,其余成员保持原有顺序。
// 在动态排序(或配置顺序)的结果上应用:置顶成员压过所有档位,只要不在冷却期就最先被尝试;
// 冷却判断留给调用方主循环——置顶成员冷却中会被跳过,流量自然落到其后的成员,冷却结束自动恢复置顶。
// pinned 中不在 members 里的项忽略。
func reorderPinnedFirst(pinned, members []string) []string {
	if len(pinned) == 0 || len(members) == 0 {
		return members
	}
	pinnedSet := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		pinnedSet[p] = true
	}
	head := make([]string, 0, len(members))
	rest := make([]string, 0, len(members))
	for _, m := range members {
		if pinnedSet[m] {
			head = append(head, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(head, rest...)
}

// activePins 返回 group 当前生效的置顶成员(按配置顺序),以及每个成员的剩余条件说明
// (如 "剩23分钟" / "剩57次",永久置顶无说明)。
// 时长到期或成功请求数达到上限的置顶被过滤;读时判定、不写盘——
// 请求路径零开销,过期规则留在配置里是惰性的,下次置顶/取消/编辑群组时自然清理。
func (gm *groupManager) activePins(groupName string) ([]string, map[string]string) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	cfg, ok := gm.groups[groupName]
	if !ok || len(cfg.Pinned) == 0 {
		return nil, nil
	}
	now := time.Now()
	pins := make([]string, 0, len(cfg.Pinned))
	notes := make(map[string]string, len(cfg.Pinned))
	for _, p := range cfg.Pinned {
		rule, has := cfg.PinRules[p]
		if has {
			if rule.Until != "" {
				exp, ok := parseExpires(rule.Until)
				if !ok || now.After(exp) {
					continue // 时长已到期
				}
			}
			if rule.MaxReqs > 0 && successCount(gm.states[p])-rule.BaseReqs >= rule.MaxReqs {
				continue // 成功请求数已达上限
			}
		}
		pins = append(pins, p)
		if note := pinRuleNote(rule, has, now, gm.states[p]); note != "" {
			notes[p] = note
		}
	}
	if len(pins) == 0 {
		return nil, notes
	}
	return pins, notes
}

// pinRuleNote 生成置顶剩余条件的简短说明("剩23分钟" / "剩57次" / "剩1.5小时/57次")。
// 无规则(永久置顶)返回空。
func pinRuleNote(rule PinRule, has bool, now time.Time, st *memberState) string {
	if !has {
		return ""
	}
	var parts []string
	if rule.Until != "" {
		if exp, ok := parseExpires(rule.Until); ok {
			d := exp.Sub(now)
			if d < 0 {
				d = 0
			}
			if d >= time.Hour {
				parts = append(parts, fmt.Sprintf("剩%.1f小时", d.Hours()))
			} else {
				parts = append(parts, fmt.Sprintf("剩%d分钟", int(d.Minutes())+1))
			}
		}
	}
	if rule.MaxReqs > 0 {
		left := rule.MaxReqs - (successCount(st) - rule.BaseReqs)
		if left < 0 {
			left = 0
		}
		parts = append(parts, fmt.Sprintf("剩%d次", left))
	}
	return strings.Join(parts, "/")
}

// successCount 返回成员累计成功请求次数(totalReqs 减去 >=400 的失败计数),未知成员返回 0。
func successCount(st *memberState) int64 {
	if st == nil {
		return 0
	}
	var fails int64
	for code, cnt := range st.statusCounts {
		if code >= 400 {
			fails += cnt
		}
	}
	return st.totalReqs - fails
}

// memberSuccessCount 是 successCount 的加锁版本(给 admin 置顶时取基线用)。
func (gm *groupManager) memberSuccessCount(alias string) int64 {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return successCount(gm.states[alias])
}

// filterPinRules 过滤置顶规则,只保留仍是置顶成员的项(群组编辑保存时调用)。
func filterPinRules(rules map[string]PinRule, pinned []string) map[string]PinRule {
	if len(rules) == 0 {
		return nil
	}
	set := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		set[p] = true
	}
	out := make(map[string]PinRule, len(rules))
	for alias, rule := range rules {
		if set[alias] {
			out[alias] = rule
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterPinnedMembers 过滤置顶列表,只保留仍是 members 的项。
// 群组编辑(增删成员)保存时调用,避免残留对已移除成员的置顶。
func filterPinnedMembers(pinned, members []string) []string {
	if len(pinned) == 0 {
		return nil
	}
	set := make(map[string]bool, len(members))
	for _, m := range members {
		set[m] = true
	}
	out := make([]string, 0, len(pinned))
	for _, p := range pinned {
		if set[p] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
