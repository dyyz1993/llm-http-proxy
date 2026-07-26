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

// TestFmtResetTime 验证 fmtResetTime 对 ms==0 的处理。
// ms==0 = z.ai 未返回 nextResetTime 字段(额度窗口未启动),应显示"未启动"而非"已到期"。
func TestFmtResetTime(t *testing.T) {
	cases := []struct {
		name   string
		ms     int64
		expect string
	}{
		{"ms=0(未启动)", 0, "未启动"},
		{"ms=1(1970年,真过期)", 1, "已到期"},
		{"未来24h内", time.Now().Add(2 * time.Hour).UnixMilli(), "今天"}, // 含"今天"
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fmtResetTime(c.ms)
			if !strings.Contains(got, c.expect) {
				t.Errorf("fmtResetTime(%d) = %q, 应包含 %q", c.ms, got, c.expect)
			}
		})
	}
}

// TestBuildQuotaHTML_ShowUnstarted 验证:额度窗口未启动(NextResetMs==0)的行保留显示,
// 且文案是"未启动"而非"已到期"。
// 场景:pro/max 套餐的 unit=3 有时返回 0% + 无 nextResetTime(窗口未启动),
// 这种应显示出来(每个套餐该有的额度类型都显示),不能隐藏,文案不能误导成"已到期"。
func TestBuildQuotaHTML_ShowUnstarted(t *testing.T) {
	entries := []cachedQuota{
		{
			Alias: "glm",
			Level: "max",
			Limits: []quotaLimit{
				// 5h额度:窗口未启动(0% + 无重置时间)
				{Type: "TOKENS_LIMIT", Unit: 3, Percentage: 0, NextResetMs: 0},
				// 周额度:正常
				{Type: "TOKENS_LIMIT", Unit: 6, Percentage: 1, NextResetMs: 1785587454984},
			},
			FetchedAt: time.Now(),
		},
	}
	html := buildQuotaHTML(entries, nil)

	// 5h额度 行应保留显示(不能隐藏)
	if !strings.Contains(html, "5h额度") {
		t.Errorf("未启动的 5h额度 行应保留显示,但 HTML 缺失:\n%s", html)
	}
	// 应显示"未启动",不是"已到期"
	if !strings.Contains(html, "未启动") {
		t.Errorf("未启动的额度应显示'未启动',但 HTML 缺失:\n%s", html)
	}
	if strings.Contains(html, "已到期") {
		t.Errorf("未启动的额度不应显示'已到期'(误导),但 HTML 包含它:\n%s", html)
	}
}
