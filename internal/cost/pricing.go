// Package cost 提供智谱 GLM 系列模型的费用计算功能。
//
// 定价数据基于官方公布的价格表（元/百万 tokens），支持：
//   - 按模型名自动匹配定价
//   - 按输入/输出长度分档定价（如 <32K 和 ≥32K）
//   - 缓存命中折扣价
package cost

import (
	"fmt"
	"strings"
)

// PricingTier 定义单个定价档位。
type PricingTier struct {
	MinInputTokens  int     // 最小输入 tokens（含），0 表示不限制
	MaxInputTokens  int     // 最大输入 tokens（含），0 表示不限制
	MinOutputTokens int     // 最小输出 tokens（含），0 表示不限制
	MaxOutputTokens int     // 最大输出 tokens（含），0 表示不限制
	InputPrice      float64 // 输入价格（元/百万 tokens）
	OutputPrice     float64 // 输出价格（元/百万 tokens）
	CacheHitPrice   float64 // 缓存命中价格（元/百万 tokens）
	Desc            string  // 档位描述
}

// ModelPricing 包含一个模型的所有定价信息。
type ModelPricing struct {
	Name          string        // 模型名称（标准化后）
	DisplayName   string        // 显示名称
	ContextWindow int64         // 上下文窗口大小（tokens）
	Tiers         []PricingTier // 定价档位列表
}

// matchTier 检查给定 token 数是否匹配该档位的范围。
func (t *PricingTier) match(inputTokens, outputTokens int) bool {
	if t.MinInputTokens > 0 && inputTokens < t.MinInputTokens {
		return false
	}
	if t.MaxInputTokens > 0 && inputTokens > t.MaxInputTokens {
		return false
	}
	if t.MinOutputTokens > 0 && outputTokens < t.MinOutputTokens {
		return false
	}
	if t.MaxOutputTokens > 0 && outputTokens > t.MaxOutputTokens {
		return false
	}
	return true
}

// 完整的定价数据表。
//
// 定价单位：元/百万 tokens
// 档位条件中的数字单位为 tokens
var pricingTable = []ModelPricing{
	{
		Name:          "glm-5.2",
		DisplayName:   "GLM-5.2",
		ContextWindow: 1_000_000,
		Tiers: []PricingTier{
			{
				Desc:          "统一价格",
				InputPrice:    8,
				OutputPrice:   28,
				CacheHitPrice: 2,
			},
		},
	},
	{
		Name:          "glm-5.1",
		DisplayName:   "GLM-5.1",
		ContextWindow: 128_000,
		Tiers: []PricingTier{
			{
				Desc:           "输入 < 32K",
				MaxInputTokens: 31999,
				InputPrice:     6,
				OutputPrice:    24,
				CacheHitPrice:  1.3,
			},
			{
				Desc:           "输入 ≥ 32K",
				MinInputTokens: 32000,
				InputPrice:     8,
				OutputPrice:    28,
				CacheHitPrice:  2,
			},
		},
	},
	{
		Name:          "glm-5-turbo",
		DisplayName:   "GLM-5-Turbo",
		ContextWindow: 128_000,
		Tiers: []PricingTier{
			{
				Desc:           "输入 < 32K",
				MaxInputTokens: 31999,
				InputPrice:     5,
				OutputPrice:    22,
				CacheHitPrice:  1.2,
			},
			{
				Desc:           "输入 ≥ 32K",
				MinInputTokens: 32000,
				InputPrice:     7,
				OutputPrice:    26,
				CacheHitPrice:  1.8,
			},
		},
	},
	{
		Name:          "glm-5",
		DisplayName:   "GLM-5",
		ContextWindow: 128_000,
		Tiers: []PricingTier{
			{
				Desc:           "输入 < 32K",
				MaxInputTokens: 31999,
				InputPrice:     4,
				OutputPrice:    18,
				CacheHitPrice:  1,
			},
			{
				Desc:           "输入 ≥ 32K",
				MinInputTokens: 32000,
				InputPrice:     6,
				OutputPrice:    22,
				CacheHitPrice:  1.5,
			},
		},
	},
	{
		Name:          "glm-4.7",
		DisplayName:   "GLM-4.7",
		ContextWindow: 128_000,
		Tiers: []PricingTier{
			{
				Desc:            "输入 < 32K, 输出 < 200",
				MaxInputTokens:  31999,
				MaxOutputTokens: 199,
				InputPrice:      2,
				OutputPrice:     8,
				CacheHitPrice:   0.4,
			},
			{
				Desc:            "输入 < 32K, 输出 ≥ 200",
				MaxInputTokens:  31999,
				MinOutputTokens: 200,
				InputPrice:      3,
				OutputPrice:     14,
				CacheHitPrice:   0.6,
			},
		},
	},
}

// 已知模型名列表，按匹配优先级（全名 → 长前缀 → 短前缀）排序。
var knownModels = []struct {
	keyword   string // 匹配关键词（小写）
	modelName string // 映射到的标准模型名
}{
	{"glm-5.2", "glm-5.2"},
	{"glm-5.1", "glm-5.1"},
	{"glm-5-turbo", "glm-5-turbo"},
	{"glm-5", "glm-5"},
	{"glm-4.6", "glm-4.7"}, // glm-4.6 约等于 glm-4.7,复用同一套定价
	{"glm-4.7", "glm-4.7"},
}

// ResolveModelName 将传入的模型字符串标准化为已知模型名。
func ResolveModelName(raw string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	for _, m := range knownModels {
		if strings.Contains(normalized, m.keyword) {
			return m.modelName, nil
		}
	}
	return "", fmt.Errorf("cost: 未知的模型名 %q", raw)
}

// GetPricing 根据标准化后的模型名返回对应的定价信息。
func GetPricing(modelName string) (*ModelPricing, error) {
	for i := range pricingTable {
		if pricingTable[i].Name == modelName {
			return &pricingTable[i], nil
		}
	}
	return nil, fmt.Errorf("cost: 找不到模型 %q 的定价信息", modelName)
}

// FindTier 根据输入/输出 token 数在定价中查找匹配的档位。
func (p *ModelPricing) FindTier(inputTokens, outputTokens int) (*PricingTier, error) {
	for i := range p.Tiers {
		if p.Tiers[i].match(inputTokens, outputTokens) {
			return &p.Tiers[i], nil
		}
	}
	return nil, fmt.Errorf(
		"cost: 模型 %s 没有匹配 input=%d output=%d 的定价档位",
		p.DisplayName, inputTokens, outputTokens,
	)
}
