package main

import (
	"testing"
)

// helper:构造一个带配额条目的 quotaCache
func newTestQuotaCache(entries []cachedQuota) *quotaCache {
	qc := newQuotaCache(":0")
	qc.entries = entries
	return qc
}

// helper:构造一个 limit
func lim(unit int, pct int, resetMs int64) quotaLimit {
	return quotaLimit{Unit: unit, Percentage: pct, NextResetMs: resetMs}
}

// TestSortGroupMembersDynamic_NilCache qc==nil 时原样返回,不排序不 panic。
func TestSortGroupMembersDynamic_NilCache(t *testing.T) {
	members := []string{"a", "b", "c"}
	got := sortGroupMembersDynamic(members, nil, 5)
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("qc==nil 应原样返回, got %v", got)
	}
}

// TestSortGroupMembersDynamic_UnstartedFirst 验证核心逻辑:
// 窗口未启动(resetTime==0, percentage==0)的无周额度成员应排最前,优先触发真实流量激活;
// 已启动的按到期时间正常排队。
func TestSortGroupMembersDynamic_UnstartedFirst(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "started", Level: "pro", Limits: []quotaLimit{lim(3, 50, 1000)}}, // 已启动
		{Alias: "unstarted", Level: "pro", Limits: []quotaLimit{lim(3, 0, 0)}},   // 未启动(应排最前)
	})
	// offset=0:未启动排最前(合成 -1),已启动排后
	got := sortGroupMembersDynamic([]string{"started", "unstarted"}, qc, 0)
	if len(got) != 2 {
		t.Fatalf("应有 2 个成员, got %v", got)
	}
	if got[0] != "unstarted" || got[1] != "started" {
		t.Errorf("未启动成员应排最前优先触发, got %v", got)
	}
}

// TestSortGroupMembersDynamic_Rotation offset 轮询偏移正确。
// 3 个已启动成员,offset 在 0/1/2 之间轮换,起点应分别是 [0],[1],[2]。
func TestSortGroupMembersDynamic_Rotation(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "a", Level: "pro", Limits: []quotaLimit{lim(3, 10, 100)}}, // resetTime 最小
		{Alias: "b", Level: "pro", Limits: []quotaLimit{lim(3, 20, 200)}},
		{Alias: "c", Level: "pro", Limits: []quotaLimit{lim(3, 30, 300)}}, // resetTime 最大
	})
	members := []string{"a", "b", "c"}

	// 升序排列后是 [a, b, c](按 resetTime)
	// offset=0 → [a,b,c]; offset=1 → [b,c,a]; offset=2 → [c,a,b]
	cases := []struct {
		offset int64
		first  string
	}{
		{0, "a"},
		{1, "b"},
		{2, "c"},
		{3, "a"}, // 回绕
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
		{Alias: "pro1", Level: "pro", Limits: []quotaLimit{lim(3, 50, 1000)}},              // 无周额度
		{Alias: "max1", Level: "max", Limits: []quotaLimit{lim(3, 0, 0), lim(6, 1, 5000)}}, // 有周额度
	})
	// max1 有周额度 → 进 deferred 排最后,不参与轮询
	got := sortGroupMembersDynamic([]string{"pro1", "max1"}, qc, 0)
	if len(got) != 2 {
		t.Fatalf("应有 2 个成员, got %v", got)
	}
	if got[1] != "max1" {
		t.Errorf("有周额度的成员应排最后(deferred), got %v", got)
	}
	// 即使轮询偏移,max1 也不参与轮询(始终在 deferred 最后)
	got = sortGroupMembersDynamic([]string{"pro1", "max1"}, qc, 1)
	if got[1] != "max1" {
		t.Errorf("有周额度的成员不参与轮询,应恒在最后, got %v", got)
	}
}

// TestSortGroupMembersDynamic_UnknownMemberFirst 缓存里没有的成员(未知)按未启动处理,排最前。
// 未知成员 entry 不存在 → hasWeekly=false, resetTime=0 → 进 scores 合成 -1 排最前
// (和窗口未启动同档,优先触发流量)。
func TestSortGroupMembersDynamic_UnknownMemberFirst(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "known", Level: "pro", Limits: []quotaLimit{lim(3, 50, 1000)}},
	})
	got := sortGroupMembersDynamic([]string{"known", "unknown"}, qc, 0)
	if len(got) != 2 {
		t.Fatalf("应有 2 个成员, got %v", got)
	}
	// unknown(resetTime=0→-1) 排最前, known(resetTime=1000) 排后
	if got[0] != "unknown" || got[1] != "known" {
		t.Errorf("未知成员按未启动处理应排最前, got %v", got)
	}
}
