package cost

import "math"

// CostResult 包含一次请求的费用计算结果。
type CostResult struct {
	ModelName    string // 模型名称
	InputTokens  int    // 输入 tokens
	OutputTokens int    // 输出 tokens
	TotalTokens  int    // 总 tokens
	CacheHit     bool   // 是否缓存命中

	InputPricePerMillion  float64 // 输入单价（元/百万 tokens）
	OutputPricePerMillion float64 // 输出单价（元/百万 tokens）
	InputCost             float64 // 输入费用（元）
	OutputCost            float64 // 输出费用（元）
	TotalCost             float64 // 总费用（元）
	TierDesc              string  // 使用的定价档位描述
}

// Calculate 计算一次模型调用的费用。
//
// 参数：
//   - modelName: 模型名称（支持大小写不敏感，如 "GLM-5.1"、"glm-5.2-flash"）
//   - inputTokens: 输入 token 数（prompt_tokens）
//   - outputTokens: 输出 token 数（completion_tokens）
//   - cacheHit: 是否缓存命中（是则使用缓存命中价格计算输入部分）
//
// 返回值：
//   - *CostResult: 详细的费用结构
//   - error: 如果模型未找到或没有匹配的定价档位则返回错误
func Calculate(modelName string, inputTokens, outputTokens int, cacheHit bool) (*CostResult, error) {
	// 1. 解析模型名
	resolved, err := ResolveModelName(modelName)
	if err != nil {
		return nil, err
	}

	// 2. 获取定价
	pricing, err := GetPricing(resolved)
	if err != nil {
		return nil, err
	}

	// 3. 查找档位
	tier, err := pricing.FindTier(inputTokens, outputTokens)
	if err != nil {
		return nil, err
	}

	// 4. 计算费用
	inputPrice := tier.InputPrice
	if cacheHit {
		inputPrice = tier.CacheHitPrice
	}

	inputCost := float64(inputTokens) / 1_000_000.0 * inputPrice
	outputCost := float64(outputTokens) / 1_000_000.0 * tier.OutputPrice
	totalCost := inputCost + outputCost

	// 四舍五入到 6 位小数（分以下精度）
	inputCost = math.Round(inputCost*1e6) / 1e6
	outputCost = math.Round(outputCost*1e6) / 1e6
	totalCost = math.Round(totalCost*1e6) / 1e6

	return &CostResult{
		ModelName:    pricing.DisplayName,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		CacheHit:     cacheHit,

		InputPricePerMillion:  inputPrice,
		OutputPricePerMillion: tier.OutputPrice,
		InputCost:             inputCost,
		OutputCost:            outputCost,
		TotalCost:             totalCost,
		TierDesc:              tier.Desc,
	}, nil
}

// MustCalculate 是 Calculate 的便捷封装，遇到错误时 panic。
func MustCalculate(modelName string, inputTokens, outputTokens int, cacheHit bool) *CostResult {
	r, err := Calculate(modelName, inputTokens, outputTokens, cacheHit)
	if err != nil {
		panic(err)
	}
	return r
}
