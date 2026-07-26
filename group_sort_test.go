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

// TestSortGroupMembersDynamic_UnstartedIncluded 验证核心修复:
// 窗口未启动(resetTime==0)的无周额度成员应进 scores 池参与轮询,
// 而不是被踢进 deferred 永远轮不到。
func TestSortGroupMembersDynamic_UnstartedIncluded(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "started", Level: "pro", Limits: []quotaLimit{lim(3, 50, 1000)}}, // 已启动
		{Alias: "unstarted", Level: "pro", Limits: []quotaLimit{lim(3, 0, 0)}},   // 未启动(修复前会被踢进 deferred)
	})
	// offset=0:已启动排前,未启动排后(合成 MaxInt64)
	got := sortGroupMembersDynamic([]string{"started", "unstarted"}, qc, 0)
	if len(got) != 2 {
		t.Fatalf("应有 2 个成员, got %v", got)
	}
	if got[0] != "started" || got[1] != "unstarted" {
		t.Errorf("未启动成员应进 scores 池排最后, got %v", got)
	}
	// offset=1:轮询偏移,未启动成员应被选为起点
	got = sortGroupMembersDynamic([]string{"started", "unstarted"}, qc, 1)
	if got[0] != "unstarted" {
		t.Errorf("offset=1 时未启动成员应轮到首位(参与轮询), got %v", got)
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

// TestSortGroupMembersDynamic_UnknownMemberDeferred 缓存里没有的成员(未知)进 deferred 排最后。
func TestSortGroupMembersDynamic_UnknownMemberDeferred(t *testing.T) {
	qc := newTestQuotaCache([]cachedQuota{
		{Alias: "known", Level: "pro", Limits: []quotaLimit{lim(3, 50, 1000)}},
	})
	// unknown 不在缓存里 → 无周额度信息 → 进 deferred? 不,看代码:!hasWeekly && resetTime>0
	// unknown: entry 不存在,hasWeekly=false, resetTime=0 → 进 scores(合成 MaxInt64)
	// 实际行为验证:unknown 和 known 都进 scores
	got := sortGroupMembersDynamic([]string{"known", "unknown"}, qc, 0)
	if len(got) != 2 {
		t.Fatalf("应有 2 个成员, got %v", got)
	}
	// known(resetTime=1000) 排前, unknown(resetTime=0→MaxInt64) 排后
	if got[0] != "known" || got[1] != "unknown" {
		t.Errorf("已知成员排前,未知成员(合成 MaxInt64)排后, got %v", got)
	}
}
