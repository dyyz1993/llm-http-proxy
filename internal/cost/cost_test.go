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
	r, err := Calculate("GLM-5.2", 1000, 200, false)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(r.TotalCost, 0.008+0.0056) {
		t.Errorf("TotalCost = %.6f, want %.6f", r.TotalCost, 0.0136)
	}
}

func TestCalculate_GLM52_CacheHit(t *testing.T) {
	r, err := Calculate("GLM-5.2", 1000, 200, true)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(r.TotalCost, 0.002+0.0056) {
		t.Errorf("TotalCost = %.6f, want %.6f", r.TotalCost, 0.0076)
	}
}

func TestCalculate_GLM51_Tier(t *testing.T) {
	// < 32K
	r, err := Calculate("GLM-5.1", 15000, 500, false)
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
	r, err = Calculate("GLM-5.1", 50000, 1000, false)
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
	r, err := Calculate("GLM-5.2", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalCost != 0 {
		t.Errorf("TotalCost = %.6f, want 0", r.TotalCost)
	}
}

func TestCalculate_UnknownModel(t *testing.T) {
	_, err := Calculate("claude-3-opus", 100, 50, false)
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}
