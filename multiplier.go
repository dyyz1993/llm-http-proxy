package main

import (
	"path"
	"strings"
	"time"
)

// TokenMultiplierRule 定义 Token 用量乘数规则。
// 在 keys.yaml 的 token_multipliers 段配置。
//
// 匹配语义:
//   - Models / Domains 为空 = 不限制(匹配所有)
//   - 非空时用 path.Match 做 glob 匹配,大小写不敏感
//   - 同一维度的多个 pattern 是 OR 关系
//   - Models + Domains 同时非空时为 AND(必须都匹配)
//   - TimeBlock 非空时,只在指定时段内生效
//   - 多个规则同时命中时,乘数相乘
type TokenMultiplierRule struct {
	Models    []string   `yaml:"models"`
	Domains   []string   `yaml:"domains"`
	Multiply  float64    `yaml:"multiply"`
	TimeBlock *TimeBlock `yaml:"time_block,omitempty"` // 可选:只在此时段内生效
}

// applyTokenMultiplier 根据模型名称、域名和当前时间计算乘数。
// 没有任何规则命中时返回 1.0。
// 规则中的 TimeBlock 非空时,仅当当前时间在时段内才命中。
func applyTokenMultiplier(rules []TokenMultiplierRule, model, domain string) float64 {
	return applyTokenMultiplierAt(rules, model, domain, time.Now())
}

// applyTokenMultiplierAt 和 applyTokenMultiplier 相同,但允许指定时间(方便测试)。
func applyTokenMultiplierAt(rules []TokenMultiplierRule, model, domain string, now time.Time) float64 {
	mult := 1.0
	for _, rule := range rules {
		// 时间限制:TimeBlock 非空且当前不在时段内 → 跳过
		if rule.TimeBlock != nil && !rule.TimeBlock.IsBlocked(now) {
			continue
		}
		if len(rule.Domains) > 0 && !globMatchAny(domain, rule.Domains) {
			continue
		}
		if len(rule.Models) > 0 && !globMatchAny(model, rule.Models) {
			continue
		}
		mult *= rule.Multiply
	}
	return mult
}

// globMatchAny 检查 s 是否匹配 patterns 中任意一个 glob pattern。
// 大小写不敏感,底层使用 path.Match。
func globMatchAny(s string, patterns []string) bool {
	s = strings.ToLower(s)
	for _, p := range patterns {
		matched, _ := path.Match(strings.ToLower(p), s)
		if matched {
			return true
		}
	}
	return false
}
