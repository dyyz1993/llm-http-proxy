package main

import (
	"testing"
	"time"
)

// helper:构造一个带配额条目的 quotaCache
func newTestQuotaCache(entries []cachedQuota) *quotaCache {
	qc := newQuotaCache(":0")
	qc.entries = entries
	return qc
}

// helper:构造一个 limit。resetMs 用绝对毫秒时间戳;
// 便捷函数 futureReset(min) = 现在 + min 分钟(避免测试里写死时间戳)
func lim(unit, pct int, resetMs int64) quotaLimit {
	return quotaLimit{Unit: unit, Percentage: pct, NextResetMs: resetMs}
}

func futureReset(min int) int64 {
	return time.Now().Add(time.Duration(min) * time.Minute).UnixMilli()
}

// TestSortGroupMembersDynamic_NilCache qc==nil 时原样返回,不排序不 panic。
func TestSortGroupMembersDynamic_NilCache(t *testing.T) {
	members := []string{"a", "b", "c"}
	got := sortGroupMembersDynamic(members, nil, 5)
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("qc==nil 应原样返回, got %v", got)
	}
}

// TestSortGroupMembersDynamic_UnstartedFirst 窗口未启动(percentage==0 且无 reset)
// 进 tier 0 排最前,优先触发激活。
func TestSortGroupMembersDynamic_UnstartedFirst(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "started", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}},
		{Alias: "unstarted", Level: "pro", Limits: []quotaLimit{lim(3, 0, 0)}},
	})
	got := sortGroupMembersDynamic([]string{"started", "unstarted"}, qc, 0)
	if len(got) != 2 || got[0] != "unstarted" || got[1] != "started" {
		t.Errorf("未启动成员应排最前(tier 0), got %v", got)
	}
}

// TestSortGroupMembersDynamic_Rotation offset 轮询偏移只对 tier 2(正常档)生效。
// 3 个正常成员(percentage 低 + 不快到期),offset 在 0/1/2 之间轮换起点。
func TestSortGroupMembersDynamic_Rotation(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "a", Level: "pro", Limits: []quotaLimit{lim(3, 10, futureReset(100))}},
		{Alias: "b", Level: "pro", Limits: []quotaLimit{lim(3, 20, futureReset(200))}},
		{Alias: "c", Level: "pro", Limits: []quotaLimit{lim(3, 30, futureReset(300))}},
	})
	members := []string{"a", "b", "c"}

	// 升序后是 [a, b, c](按 resetTime,都在 tier 2)
	cases := []struct {
		offset int64
		first  string
	}{
		{0, "a"},
		{1, "b"},
		{2, "c"},
		{3, "a"},
	}
	for _, c := range cases {
		got := sortGroupMembersDynamic(members, qc, c.offset)
		if got[0] != c.first {
			t.Errorf("offset=%d: 起点应是 %s, got %s (full: %v)", c.offset, c.first, got[0], got)
		}
	}
}

// TestSortGroupMembersDynamic_WeeklyDeferred 有周额度的成员进 deferred 排最后。
func TestSortGroupMembersDynamic_WeeklyDeferred(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "pro1", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}},
		{Alias: "max1", Level: "max", Limits: []quotaLimit{lim(3, 0, 0), lim(6, 1, futureReset(5000))}},
	})
	got := sortGroupMembersDynamic([]string{"pro1", "max1"}, qc, 0)
	if len(got) != 2 || got[1] != "max1" {
		t.Errorf("有周额度的成员应排最后(deferred), got %v", got)
	}
	// 即使 offset,max1 也不参与轮询(恒在 deferred 最后)
	got = sortGroupMembersDynamic([]string{"pro1", "max1"}, qc, 1)
	if got[1] != "max1" {
		t.Errorf("deferred 成员不参与轮询, got %v", got)
	}
}

// TestSortGroupMembersDynamic_UnknownMemberFirst 缓存里没有的成员按未启动处理,排最前。
func TestSortGroupMembersDynamic_UnknownMemberFirst(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "known", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}},
	})
	got := sortGroupMembersDynamic([]string{"known", "unknown"}, qc, 0)
	if len(got) != 2 || got[0] != "unknown" {
		t.Errorf("未知成员按未启动处理应排最前(tier 0), got %v", got)
	}
}

// === v2.5.52 新增:双维度排序测试 ===

// TestSort_HighUsage90Defers 平时(不快到期)percentage>=90 让位,进 tier 3。
func TestSort_HighUsage90Defers(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "normal", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}}, // tier 2
		{Alias: "high", Level: "pro", Limits: []quotaLimit{lim(3, 92, futureReset(300))}},   // tier 3
	})
	got := sortGroupMembersDynamic([]string{"high", "normal"}, qc, 0)
	if got[0] != "normal" || got[1] != "high" {
		t.Errorf("92%%(平时)应让位排后(tier 3), got %v", got)
	}
}

// TestSort_HighUsage98AlwaysDefers percentage>=98 无论如何都让位(绝对上限),
// 即使快到期也进 tier 3。
func TestSort_HighUsage98AlwaysDefers(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		// 99% + 快到期(剩5min) → 仍 tier 3(98% 绝对上限)
		{Alias: "maxed", Level: "pro", Limits: []quotaLimit{lim(3, 99, futureReset(5))}},
		{Alias: "normal", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}},
	})
	got := sortGroupMembersDynamic([]string{"maxed", "normal"}, qc, 0)
	if got[0] != "normal" || got[1] != "maxed" {
		t.Errorf("99%% 即使快到期也该让位(tier 3,绝对上限), got %v", got)
	}
}

// TestSort_UrgentBoostsTo98 快到期(剩<10min)+ percentage<98 → 解禁到 tier 1 全力。
// 关键:平时 90% 会进 tier 3,但快到期时 95% 升到 tier 1(解禁)。
func TestSort_UrgentBoostsTo98(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		// 95% + 快到期(剩5min) → tier 1(解禁全力,可用到 98%)
		{Alias: "urgent", Level: "pro", Limits: []quotaLimit{lim(3, 95, futureReset(5))}},
		// 50% + 不快到期 → tier 2(正常)
		{Alias: "normal", Level: "pro", Limits: []quotaLimit{lim(3, 50, futureReset(300))}},
	})
	got := sortGroupMembersDynamic([]string{"normal", "urgent"}, qc, 0)
	// tier 1 < tier 2,所以 urgent 排前
	if got[0] != "urgent" || got[1] != "normal" {
		t.Errorf("95%%+快到期应解禁到 tier 1 排最前, got %v", got)
	}
}

// TestSort_RotationOnlyTier2 offset 只对 tier 2 生效,tier 0/1/3 保持排序顺序。
func TestSort_RotationOnlyTier2(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		// tier 1(快到期 5%,全力)
		{Alias: "urgent", Level: "pro", Limits: []quotaLimit{lim(3, 5, futureReset(5))}},
		// 两个 tier 2(正常)
		{Alias: "a", Level: "pro", Limits: []quotaLimit{lim(3, 10, futureReset(100))}},
		{Alias: "b", Level: "pro", Limits: []quotaLimit{lim(3, 20, futureReset(200))}},
		// tier 3(平时让位)
		{Alias: "high", Level: "pro", Limits: []quotaLimit{lim(3, 92, futureReset(300))}},
	})
	// offset=1:只对 tier 2 (a,b) 做轮换 → (b,a)
	// tier 1 (urgent) 恒在最前,tier 3 (high) 恒在 tier 2 后
	got := sortGroupMembersDynamic([]string{"urgent", "a", "b", "high"}, qc, 1)
	want := []string{"urgent", "b", "a", "high"}
	if len(got) != 4 {
		t.Fatalf("应有 4 个成员, got %v", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("位置 %d 应是 %s, got %s (full: %v)", i, w, got[i], got)
			break
		}
	}
}
