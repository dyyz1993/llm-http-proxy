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
	"sync"
	"time"
)

// GroupConfig 是单个 group 的配置。
type GroupConfig struct {
	Members  []string `yaml:"members"`   // 成员 alias 列表(按优先级排序)
	OnStatus []int    `yaml:"on_status"` // 上游返回这些码时触发换人
	Cooldown string   `yaml:"cooldown"`  // 冷却时间(如 "5m", "30s")
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
	mu     sync.RWMutex
	groups map[string]GroupConfig  // group 名 → 配置
	states map[string]*memberState // alias → 状态(所有 group 共享一份)
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
	gm.mu.Lock()
	defer gm.mu.Unlock()

	cooldown := gm.parseCooldown(groupName)

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
