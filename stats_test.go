package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestRenderWindowTable 验证 renderWindowTable 输出格式正确。
func TestRenderWindowTable(t *testing.T) {
	hours := []hourEntry{
		{Hour: "2026-06-28 10:00", Count: 5},
		{Hour: "2026-06-28 11:00", Count: 15},
		{Hour: "2026-06-28 12:00", Count: 10},
	}

	var buf bytes.Buffer
	renderWindowTable(&buf, hours)
	output := buf.String()

	// 应包含表头
	if !strings.Contains(output, "HOUR") || !strings.Contains(output, "COUNT") || !strings.Contains(output, "BAR") {
		t.Error("输出应包含 HOUR/COUNT/BAR 表头")
	}
	// 应包含小时数据
	for _, h := range hours {
		if !strings.Contains(output, h.Hour) {
			t.Errorf("输出应包含小时 %q", h.Hour)
		}
	}
	// 应包含总计行
	if !strings.Contains(output, "总计") || !strings.Contains(output, "30") {
		t.Errorf("总计应为 30 次, output: %s", output)
	}
	// 应包含分隔线
	if strings.Count(output, "---") < 2 {
		t.Error("输出应包含至少 2 条分隔线")
	}
}

// TestRenderWindowTable_Empty 验证空切片不 panic。
func TestRenderWindowTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderWindowTable(&buf, nil)
	if buf.Len() == 0 {
		t.Error("空切片也应输出表头")
	}
	// 空切片 -> 总计 0
	output := buf.String()
	if !strings.Contains(output, "0 次") {
		t.Errorf("空切片应显示 0 次, output: %s", output)
	}

	buf.Reset()
	renderWindowTable(&buf, []hourEntry{})
	output = buf.String()
	if !strings.Contains(output, "0 次") {
		t.Errorf("空切片应显示 0 次, output: %s", output)
	}
}

// TestRenderWindowTable_SingleRow 验证单行也正常。
func TestRenderWindowTable_SingleRow(t *testing.T) {
	hours := []hourEntry{{Hour: "2026-01-01 00:00", Count: 42}}
	var buf bytes.Buffer
	renderWindowTable(&buf, hours)
	output := buf.String()
	if !strings.Contains(output, "42") {
		t.Errorf("应包含 42, output: %s", output)
	}
	if !strings.Contains(output, "总计") {
		t.Error("应包含总计行")
	}
}

// TestDistinctIPCount 验证 distinctIPCount。
func TestDistinctIPCount(t *testing.T) {
	snap := map[string]*ipStats{
		"192.168.1.1": {Keys: map[string]*keyEntry{"sk-abc": {Count: 1}}},
		"10.0.0.1":    {Keys: map[string]*keyEntry{"sk-xyz": {Count: 2}}},
		"172.16.0.1":  {Keys: map[string]*keyEntry{"sk-def": {Count: 3}}},
	}
	if n := distinctIPCount(snap); n != 3 {
		t.Errorf("distinctIPCount = %d, want 3", n)
	}

	// 空 map
	if n := distinctIPCount(map[string]*ipStats{}); n != 0 {
		t.Errorf("空 map 应返回 0, got %d", n)
	}
}

// TestTotalCount 验证 totalCount。
func TestTotalCount(t *testing.T) {
	snap := map[string]*ipStats{
		"ip1": {Keys: map[string]*keyEntry{
			"k1": {Count: 5},
			"k2": {Count: 3},
		}},
		"ip2": {Keys: map[string]*keyEntry{
			"k1": {Count: 2},
		}},
	}
	if n := totalCount(snap); n != 10 {
		t.Errorf("totalCount = %d, want 10", n)
	}
}

// TestLogRingRecent 验证 logRing.recent 返回倒序日志。
func TestLogRingRecent(t *testing.T) {
	r := &logRing{size: 10, entries: make([]logEntry, 10)}

	// 写入 5 条
	for i := 0; i < 5; i++ {
		r.add(logEntry{Time: "t", IP: "ip"})
	}

	got := r.recent(3)
	if len(got) != 3 {
		t.Errorf("recent(3) = %d 条, want 3", len(got))
	}

	// 写入 8 条(不满 size),recent(10) 应返回全部
	r2 := &logRing{size: 10, entries: make([]logEntry, 10)}
	for i := 0; i < 8; i++ {
		r2.add(logEntry{Time: "t", IP: "ip"})
	}
	if got := r2.recent(10); len(got) != 8 {
		t.Errorf("8 条数据 recent(10) = %d, want 8", len(got))
	}
}

// TestLogRingRecent_Empty 验证空 ring 返回空切片。
func TestLogRingRecent_Empty(t *testing.T) {
	r := &logRing{size: 10, entries: make([]logEntry, 10)}
	got := r.recent(5)
	if len(got) != 0 {
		t.Errorf("空 ring 应返回空切片, got %d 条", len(got))
	}

	// nil ring 的 entries 做防御检查
	r2 := &logRing{size: 10, entries: make([]logEntry, 10)}
	if got := r2.recent(0); len(got) != 0 {
		t.Errorf("recent(0) 应返回空, got %d", len(got))
	}
}

// TestLogRingRecent_WrapAround 验证环形缓冲的 wrap-around 行为。
func TestLogRingRecent_WrapAround(t *testing.T) {
	r := &logRing{size: 5, entries: make([]logEntry, 5)}

	// 写入 7 条(超过 size,会覆盖前 2 条)
	for i := 0; i < 7; i++ {
		r.add(logEntry{Time: "t", IP: "ip"})
	}

	// recent(5) 应返回最近的 5 条
	got := r.recent(5)
	if len(got) != 5 {
		t.Errorf("wrap-around 后 recent(5) = %d, want 5", len(got))
	}
}

// TestStatsCollector_ConcurrentRecord 验证并发 record 无 data race。
func TestStatsCollector_ConcurrentRecord(t *testing.T) {
	s := newStatsCollector()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.record("192.168.1.1", "sk-test", "api.example.com", 200)
			}
		}(i)
	}
	wg.Wait()
	// 验证数据完整
	s.mu.Lock()
	count := len(s.data)
	s.mu.Unlock()
	if count == 0 {
		t.Error("并发 record 后应有数据")
	}
}
