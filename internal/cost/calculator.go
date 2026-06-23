package cost

import "math"

// CostResult 包含一次请求的费用计算结果。
type CostResult struct {
	ModelName    string // 模型名称
	InputTokens  int    // 输入 tokens（总输入，含缓存命中部分）
	CachedTokens int    // 缓存命中的输入 tokens
	OutputTokens int    // 输出 tokens
	TotalTokens  int    // 总 tokens

	InputPricePerMillion    float64 // 未命中部分的输入单价（元/百万 tokens）
	CacheHitPricePerMillion float64 // 缓存命中部分的输入单价（元/百万 tokens）
	OutputPricePerMillion   float64 // 输出单价（元/百万 tokens）
	InputCost               float64 // 输入费用（元，混合计费：未命中 + 命中）
	OutputCost              float64 // 输出费用（元）
	TotalCost               float64 // 总费用（元）
	TierDesc                string  // 使用的定价档位描述
}

// Calculate 计算一次模型调用的费用。
//
// 参数：
//   - modelName: 模型名称（支持大小写不敏感，如 "GLM-5.1"、"glm-5.2-flash"）
//   - inputTokens: 总输入 token 数（prompt_tokens，含缓存命中部分）
//   - outputTokens: 输出 token 数（completion_tokens）
//   - cachedTokens: 缓存命中的输入 token 数（cached_tokens）
//
// 输入费用采用混合计费（GLM 官方规则）：
//
//	输入费用 = (inputTokens - cachedTokens) × 标准输入单价
//	         + cachedTokens × 缓存命中单价
//
// 返回值：
//   - *CostResult: 详细的费用结构
//   - error: 如果模型未找到或没有匹配的定价档位则返回错误
func Calculate(modelName string, inputTokens, outputTokens, cachedTokens int) (*CostResult, error) {
	// 0. 参数保护：cachedTokens 不能超过 inputTokens
	// （某些上游可能返回 cached > prompt 的异常数据，钳制到 inputTokens）
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}

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

	// 3. 查找档位（按总输入 token 数定档，缓存不影响分档）
	tier, err := pricing.FindTier(inputTokens, outputTokens)
	if err != nil {
		return nil, err
	}

	// 4. 混合计费：未命中部分按标准输入价，命中部分按缓存优惠价
	uncachedTokens := inputTokens - cachedTokens
	inputCost := float64(uncachedTokens)/1_000_000.0*tier.InputPrice +
		float64(cachedTokens)/1_000_000.0*tier.CacheHitPrice
	outputCost := float64(outputTokens) / 1_000_000.0 * tier.OutputPrice
	totalCost := inputCost + outputCost

	// 四舍五入到 6 位小数（分以下精度）
	inputCost = math.Round(inputCost*1e6) / 1e6
	outputCost = math.Round(outputCost*1e6) / 1e6
	totalCost = math.Round(totalCost*1e6) / 1e6

	return &CostResult{
		ModelName:    pricing.DisplayName,
		InputTokens:  inputTokens,
		CachedTokens: cachedTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,

		InputPricePerMillion:    tier.InputPrice,
		CacheHitPricePerMillion: tier.CacheHitPrice,
		OutputPricePerMillion:   tier.OutputPrice,
		InputCost:               inputCost,
		OutputCost:              outputCost,
		TotalCost:               totalCost,
		TierDesc:                tier.Desc,
	}, nil
}

// MustCalculate 是 Calculate 的便捷封装，遇到错误时 panic。
func MustCalculate(modelName string, inputTokens, outputTokens, cachedTokens int) *CostResult {
	r, err := Calculate(modelName, inputTokens, outputTokens, cachedTokens)
	if err != nil {
		panic(err)
	}
	return r
}
