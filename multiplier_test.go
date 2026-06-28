package main

import (
	"math"
	"testing"
)

// roundTo 辅助:将乘数舍入到给定精度(避免浮点比较问题)。
func roundTo(v float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(v*pow) / pow
}

func near(a, b float64) bool {
	return math.Abs(a-b) < 0.0001
}

func TestGlobMatchAny(t *testing.T) {
	tests := []struct {
		s        string
		patterns []string
		want     bool
	}{
		{"glm-5", []string{"glm-5"}, true},
		{"glm-5.1-flash", []string{"glm-5*"}, true},
		{"glm-5.1-flash", []string{"*-5.*"}, true},
		{"glm-4-plus", []string{"glm-5*"}, false},
		{"GLM-5.1-FLASH", []string{"glm-5*"}, true},           // 大小写不敏感
		{"open.bigmodel.cn", []string{"*.bigmodel.cn"}, true}, // 域名 glob
		{"open.bigmodel.cn", []string{"*.example.com"}, false},
		{"api.anthropic.com", []string{"*.anthropic.com"}, true},
		{"", []string{"*"}, true},           // 空字符串匹配 *
		{"abc", []string{"a*", "b*"}, true}, // OR:多个 pattern
		{"xyz", []string{"a*", "b*"}, false},
		{"glm-5", []string(nil), false},  // nil slice 不应匹配
		{"glm-5", []string{}, false},     // 空 slice 不应匹配
		{"cat", []string{"ca[t]"}, true}, // path.Match 字符类:ca[t] 匹配 cat
	}
	for _, tt := range tests {
		t.Run(tt.s+"|"+stringsJoin(tt.patterns, ","), func(t *testing.T) {
			got := globMatchAny(tt.s, tt.patterns)
			if got != tt.want {
				t.Errorf("globMatchAny(%q, %v) = %v, want %v", tt.s, tt.patterns, got, tt.want)
			}
		})
	}
}

// stringsJoin 避免引入 strings 包(该文件用 path 和 strings 但测试里可引入)。
func stringsJoin(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	s := ss[0]
	for _, v := range ss[1:] {
		s += sep + v
	}
	return s
}

func TestApplyTokenMultiplier_NoMatch(t *testing.T) {
	// 没有匹配的规则 → 1.0
	rules := []TokenMultiplierRule{
		{Models: []string{"glm-5*"}, Multiply: 3.0},
	}
	if got := applyTokenMultiplier(rules, "claude-4", ""); got != 1.0 {
		t.Errorf("不匹配应返回 1.0, got %v", got)
	}
}

func TestApplyTokenMultiplier_EmptyRules(t *testing.T) {
	// 空规则 → 1.0
	if got := applyTokenMultiplier(nil, "any", "any"); got != 1.0 {
		t.Errorf("空规则应返回 1.0, got %v", got)
	}
	if got := applyTokenMultiplier([]TokenMultiplierRule{}, "any", "any"); got != 1.0 {
		t.Errorf("空规则切片应返回 1.0, got %v", got)
	}
}

func TestApplyTokenMultiplier_ModelOnly(t *testing.T) {
	rules := []TokenMultiplierRule{
		{Models: []string{"glm-5*"}, Multiply: 3.0},
	}
	// 匹配
	if got := applyTokenMultiplier(rules, "glm-5.1-flash", ""); !near(got, 3.0) {
		t.Errorf("glm-5.1-flash 应获得 3x, got %v", got)
	}
	// 不匹配
	if got := applyTokenMultiplier(rules, "glm-4-plus", ""); !near(got, 1.0) {
		t.Errorf("glm-4-plus 应获得 1x, got %v", got)
	}
}

func TestApplyTokenMultiplier_DomainOnly(t *testing.T) {
	rules := []TokenMultiplierRule{
		{Domains: []string{"*.bigmodel.cn"}, Multiply: 2.0},
	}
	if got := applyTokenMultiplier(rules, "", "open.bigmodel.cn"); !near(got, 2.0) {
		t.Errorf("域名匹配应获得 2x, got %v", got)
	}
	if got := applyTokenMultiplier(rules, "", "api.anthropic.com"); !near(got, 1.0) {
		t.Errorf("域名不匹配应获得 1x, got %v", got)
	}
}

func TestApplyTokenMultiplier_AndCondition(t *testing.T) {
	rules := []TokenMultiplierRule{
		{
			Models:   []string{"glm-5*"},
			Domains:  []string{"open.bigmodel.cn"},
			Multiply: 4.0,
		},
	}
	// 两者都匹配 → 4x
	if got := applyTokenMultiplier(rules, "glm-5.1", "open.bigmodel.cn"); !near(got, 4.0) {
		t.Errorf("模型+域名都匹配应获得 4x, got %v", got)
	}
	// 仅模型匹配 → 1x
	if got := applyTokenMultiplier(rules, "glm-5.1", "api.anthropic.com"); !near(got, 1.0) {
		t.Errorf("仅模型匹配应 1x, got %v", got)
	}
	// 仅域名匹配 → 1x
	if got := applyTokenMultiplier(rules, "claude-4", "open.bigmodel.cn"); !near(got, 1.0) {
		t.Errorf("仅域名匹配应 1x, got %v", got)
	}
}

func TestApplyTokenMultiplier_EmptyModelsDomains(t *testing.T) {
	// Models/Domains 都空 → 匹配所有
	rules := []TokenMultiplierRule{
		{Multiply: 2.5},
	}
	if got := applyTokenMultiplier(rules, "anything", "any.domain"); !near(got, 2.5) {
		t.Errorf("空模型+空域名应匹配所有, got %v", got)
	}
}

func TestApplyTokenMultiplier_Stacking(t *testing.T) {
	// 两条规则都命中 → 乘数相乘: 3.0 * 2.0 = 6.0
	rules := []TokenMultiplierRule{
		{Models: []string{"glm-5*"}, Multiply: 3.0},
		{Domains: []string{"*.bigmodel.cn"}, Multiply: 2.0},
	}
	if got := applyTokenMultiplier(rules, "glm-5.1", "open.bigmodel.cn"); !near(got, 6.0) {
		t.Errorf("叠加应得 6.0(3*2), got %v", got)
	}
}

func TestApplyTokenMultiplier_OnlyOneMatches(t *testing.T) {
	// 只有一条规则匹配 → 只乘该规则
	rules := []TokenMultiplierRule{
		{Models: []string{"glm-5*"}, Multiply: 3.0},
		{Models: []string{"claude-4*"}, Multiply: 5.0},
	}
	if got := applyTokenMultiplier(rules, "glm-5.1", ""); !near(got, 3.0) {
		t.Errorf("仅匹配 glm-5* 应得 3x, got %v", got)
	}
	if got := applyTokenMultiplier(rules, "claude-4-opus", ""); !near(got, 5.0) {
		t.Errorf("仅匹配 claude-4* 应得 5x, got %v", got)
	}
}

func TestApplyTokenMultiplier_ZeroMultiply(t *testing.T) {
	// multiply=0 → 免费(不计费)
	rules := []TokenMultiplierRule{
		{Models: []string{"free-model"}, Multiply: 0},
	}
	if got := applyTokenMultiplier(rules, "free-model", ""); !near(got, 0) {
		t.Errorf("multiply=0 应返回 0, got %v", got)
	}
}

func TestApplyTokenMultiplier_CaseInsensitiveModel(t *testing.T) {
	rules := []TokenMultiplierRule{
		{Models: []string{"GLM-5*"}, Multiply: 3.0},
	}
	if got := applyTokenMultiplier(rules, "glm-5.1-flash", ""); !near(got, 3.0) {
		t.Errorf("大小写不敏感:GLM-5* 应匹配 glm-5.1-flash, got %v", got)
	}
}

func TestApplyTokenMultiplier_CaseInsensitiveDomain(t *testing.T) {
	rules := []TokenMultiplierRule{
		{Domains: []string{"*.BIGMODEL.CN"}, Multiply: 2.0},
	}
	if got := applyTokenMultiplier(rules, "", "open.bigmodel.cn"); !near(got, 2.0) {
		t.Errorf("大小写不敏感:*.BIGMODEL.CN 应匹配 open.bigmodel.cn, got %v", got)
	}
}
