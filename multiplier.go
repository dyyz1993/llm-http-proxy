package main

import (
	"path"
	"strings"
)

// TokenMultiplierRule 定义 Token 用量乘数规则。
// 在 keys.yaml 的 token_multipliers 段配置。
//
// 匹配语义:
//   - Models / Domains 为空 = 不限制(匹配所有)
//   - 非空时用 path.Match 做 glob 匹配,大小写不敏感
//   - 同一维度的多个 pattern 是 OR 关系
//   - Models + Domains 同时非空时为 AND(必须都匹配)
//   - 多个规则同时命中时,乘数相乘
type TokenMultiplierRule struct {
	Models   []string `yaml:"models"`
	Domains  []string `yaml:"domains"`
	Multiply float64  `yaml:"multiply"`
}

// applyTokenMultiplier 根据模型名称和域名计算乘数。
// 没有任何规则命中时返回 1.0。
func applyTokenMultiplier(rules []TokenMultiplierRule, model, domain string) float64 {
	mult := 1.0
	for _, rule := range rules {
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
