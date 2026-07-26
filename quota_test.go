package main

import (
	"strings"
	"testing"
	"time"
)

// TestHasWeeklyQuota 验证 hasWeeklyQuota:有 unit=6 返回 true,否则 false。
func TestHasWeeklyQuota(t *testing.T) {
	cases := []struct {
		name   string
		entry  cachedQuota
		expect bool
	}{
		{
			name:   "空 limits",
			entry:  cachedQuota{Alias: "k1"},
			expect: false,
		},
		{
			name:   "只有 unit=3(5h 窗口)",
			entry:  cachedQuota{Alias: "k1", Limits: []quotaLimit{{Unit: 3}}},
			expect: false,
		},
		{
			name:   "有 unit=6(周额度)",
			entry:  cachedQuota{Alias: "k1", Limits: []quotaLimit{{Unit: 3}, {Unit: 6}}},
			expect: true,
		},
		{
			name:   "max 套餐(unit=3,5,6)",
			entry:  cachedQuota{Alias: "k1", Limits: []quotaLimit{{Unit: 3}, {Unit: 5}, {Unit: 6}}},
			expect: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasWeeklyQuota(c.entry)
			if got != c.expect {
				t.Errorf("hasWeeklyQuota(%+v) = %v, want %v", c.entry, got, c.expect)
			}
		})
	}
}

// TestHasMonthlyQuota 验证 hasMonthlyQuota:有 unit=5 返回 true,否则 false。
// 关键场景:pro 套餐(unit=3 占位 + unit=5 月度)应返回 true,
// lite 套餐(只有 unit=3)应返回 false。
func TestHasMonthlyQuota(t *testing.T) {
	cases := []struct {
		name   string
		entry  cachedQuota
		expect bool
	}{
		{
			name:   "空 limits",
			entry:  cachedQuota{Alias: "k1"},
			expect: false,
		},
		{
			name:   "lite 套餐(只有 unit=3,无月度)",
			entry:  cachedQuota{Alias: "0316lite", Limits: []quotaLimit{{Unit: 3, Percentage: 51, NextResetMs: 1785045777765}}},
			expect: false,
		},
		{
			name:   "pro 套餐(unit=3 占位 + unit=5 月度)",
			entry:  cachedQuota{Alias: "old0115pro", Limits: []quotaLimit{{Unit: 3, Percentage: 0}, {Unit: 5, Percentage: 9}}},
			expect: true,
		},
		{
			name:   "max 套餐(unit=3,5,6)",
			entry:  cachedQuota{Alias: "glm", Limits: []quotaLimit{{Unit: 3}, {Unit: 5}, {Unit: 6}}},
			expect: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasMonthlyQuota(c.entry)
			if got != c.expect {
				t.Errorf("hasMonthlyQuota(%+v) = %v, want %v", c.entry, got, c.expect)
			}
		})
	}
}

// TestBuildQuotaHTML_SkipPlaceholder 验证占位行(NextResetMs==0 && Percentage==0)被跳过。
// pro 套餐的 unit=3 是 z.ai 返回的占位字段:永远无 nextResetTime、永远 0%,
// 渲染出来会误导成"已到期"。改后这类行不应出现在 HTML 里。
func TestBuildQuotaHTML_SkipPlaceholder(t *testing.T) {
	entries := []cachedQuota{
		{
			Alias: "old0115pro",
			Level: "pro",
			Limits: []quotaLimit{
				// 占位行:无重置时间 + 0% → 应被跳过,不显示
				{Type: "TOKENS_LIMIT", Unit: 3, Percentage: 0, NextResetMs: 0},
				// 真实行:月度时长,有数据 → 应显示
				{Type: "TIME_LIMIT", Unit: 5, Percentage: 9, NextResetMs: 1786589520980,
					Usage: intPtr(1000), CurrentVal: intPtr(96)},
			},
			FetchedAt: time.Now(),
		},
	}
	html := buildQuotaHTML(entries, nil)

	// 占位的 5h额度 不应出现(它被跳过了)
	if strings.Contains(html, "5h额度") {
		t.Errorf("占位的 5h额度 行不应渲染,但 HTML 包含它:\n%s", html)
	}
	// 月度时长 应正常显示
	if !strings.Contains(html, "月度时长") {
		t.Errorf("月度时长 行应正常渲染,但 HTML 缺失:\n%s", html)
	}
	// "已到期" 不应出现(这是旧行为的误导文案)
	if strings.Contains(html, "已到期") {
		t.Errorf("不应显示'已到期'(占位行已跳过),但 HTML 包含它:\n%s", html)
	}
}

// TestBuildQuotaHTML_KeepActiveWindow 验证:NextResetMs==0 但 Percentage>0 的行保留。
// 边界场景:窗口刚激活、第一次查询时可能短暂无重置时间但有用量,不能误删。
func TestBuildQuotaHTML_KeepActiveWindow(t *testing.T) {
	entries := []cachedQuota{
		{
			Alias: "edgecase",
			Level: "lite",
			Limits: []quotaLimit{
				// 无重置时间但有用量 → 应保留(不是占位)
				{Type: "TOKENS_LIMIT", Unit: 3, Percentage: 30, NextResetMs: 0},
			},
			FetchedAt: time.Now(),
		},
	}
	html := buildQuotaHTML(entries, nil)
	if !strings.Contains(html, "5h额度") {
		t.Errorf("有用量(30%%)的 5h额度 行应保留,但 HTML 缺失:\n%s", html)
	}
	if !strings.Contains(html, "30%") {
		t.Errorf("30%% 用量应显示,但 HTML 缺失:\n%s", html)
	}
}
