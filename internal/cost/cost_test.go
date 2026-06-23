package cost

import (
	"testing"
)

func closeEnough(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 0.0001
}

func TestResolveModelName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"GLM-5.2", "glm-5.2"},
		{"GLM-5.2-flash", "glm-5.2"},
		{"GLM-5.1", "glm-5.1"},
		{"glm-5.1-20251231", "glm-5.1"},
		{"GLM-5-Turbo", "glm-5-turbo"},
		{"GLM-5", "glm-5"},
		{"GLM-4.6", "glm-4.7"}, // 4.6 约等于 4.7,复用定价
		{"glm-4.6-flash", "glm-4.7"},
		{"GLM-4.5", "glm-4.7"}, // 4.5 约等于 4.7,复用定价
		{"glm-4.5-flash", "glm-4.7"},
		{"GLM-4.7", "glm-4.7"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ResolveModelName(tt.input)
			if err != nil {
				t.Fatalf("ResolveModelName(%q) err: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveModelName_Unknown(t *testing.T) {
	_, err := ResolveModelName("claude-3.5")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestCalculate_GLM52(t *testing.T) {
	r, err := Calculate("GLM-5.2", 1000, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(r.TotalCost, 0.008+0.0056) {
		t.Errorf("TotalCost = %.6f, want %.6f", r.TotalCost, 0.0136)
	}
}

func TestCalculate_GLM52_CacheHit(t *testing.T) {
	// 全部命中：cached == input，输入部分全部按缓存优惠价
	r, err := Calculate("GLM-5.2", 1000, 200, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(r.TotalCost, 0.002+0.0056) {
		t.Errorf("TotalCost = %.6f, want %.6f", r.TotalCost, 0.0076)
	}
}

func TestCalculate_GLM52_PartialCacheHit(t *testing.T) {
	// 部分命中：input=1000，cached=400，GLM-5.2 标准输入 8，缓存优惠 2
	// 未命中 600 × 8/1M + 命中 400 × 2/1M + 输出 200 × 28/1M
	//   = 0.0048 + 0.0008 + 0.0056 = 0.0112
	r, err := Calculate("GLM-5.2", 1000, 200, 400)
	if err != nil {
		t.Fatal(err)
	}
	wantInput := 600.0/1e6*8 + 400.0/1e6*2
	wantOutput := 200.0 / 1e6 * 28
	if !closeEnough(r.InputCost, wantInput) {
		t.Errorf("InputCost = %.6f, want %.6f", r.InputCost, wantInput)
	}
	if !closeEnough(r.OutputCost, wantOutput) {
		t.Errorf("OutputCost = %.6f, want %.6f", r.OutputCost, wantOutput)
	}
	if !closeEnough(r.TotalCost, wantInput+wantOutput) {
		t.Errorf("TotalCost = %.6f, want %.6f", r.TotalCost, wantInput+wantOutput)
	}
	// 旧逻辑（全或无）：cached>0 就把整个 prompt 按优惠价，会得到 1000×2/1M=0.002
	// 正确混合计费：0.0056，两者明显不同，证明修复生效
	if closeEnough(r.InputCost, 0.002) {
		t.Errorf("InputCost=%.6f 与旧的全或无逻辑结果相同，混合计费未生效", r.InputCost)
	}
}

func TestCalculate_GLM52_CachedExceedsInput(t *testing.T) {
	// 异常数据保护：cached > input 时钳制到 input（全命中）
	// 线上曾出现 cached=333248, prompt=37 的数据
	r, err := Calculate("GLM-5.2", 100, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	// 等价于 cached=100，全命中 → 100 × 2/1M
	if !closeEnough(r.InputCost, 100.0/1e6*2) {
		t.Errorf("InputCost = %.6f, want %.6f (cached 应被钳制到 input)", r.InputCost, 100.0/1e6*2)
	}
	if r.CachedTokens != 100 {
		t.Errorf("CachedTokens = %d, want 100 (钳制后)", r.CachedTokens)
	}
}

func TestCalculate_GLM51_Tier(t *testing.T) {
	// < 32K
	r, err := Calculate("GLM-5.1", 15000, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.TierDesc != "输入 < 32K" {
		t.Errorf("tier = %q, want 输入 < 32K", r.TierDesc)
	}
	if !closeEnough(r.TotalCost, 0.09+0.012) {
		t.Errorf("TotalCost = %.6f", r.TotalCost)
	}

	// >= 32K
	r, err = Calculate("GLM-5.1", 50000, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.TierDesc != "输入 ≥ 32K" {
		t.Errorf("tier = %q, want 输入 ≥ 32K", r.TierDesc)
	}
	if !closeEnough(r.TotalCost, 0.4+0.028) {
		t.Errorf("TotalCost = %.6f", r.TotalCost)
	}
}

func TestCalculate_ZeroTokens(t *testing.T) {
	r, err := Calculate("GLM-5.2", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalCost != 0 {
		t.Errorf("TotalCost = %.6f, want 0", r.TotalCost)
	}
}

func TestCalculate_UnknownModel(t *testing.T) {
	_, err := Calculate("claude-3-opus", 100, 50, 0)
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestCalculate_NegativeTokens(t *testing.T) {
	// 负值保护:上游可能返回异常负值,不应产生负费用
	r, err := Calculate("GLM-5.2", -100, -50, -30)
	if err != nil {
		t.Fatal(err)
	}
	if r.InputCost != 0 {
		t.Errorf("负 input 的 InputCost = %.6f, want 0", r.InputCost)
	}
	if r.OutputCost != 0 {
		t.Errorf("负 output 的 OutputCost = %.6f, want 0", r.OutputCost)
	}
	if r.TotalCost != 0 {
		t.Errorf("TotalCost = %.6f, want 0", r.TotalCost)
	}
	if r.InputTokens != 0 || r.OutputTokens != 0 {
		t.Errorf("InputTokens=%d OutputTokens=%d, want 0", r.InputTokens, r.OutputTokens)
	}
}
