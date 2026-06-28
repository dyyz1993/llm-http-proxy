package main

import (
	"testing"
)

// TestResolveConfig_NoProfile 验证没有 profile 时原样返回。
func TestResolveConfig_NoProfile(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1", MaxTokens: 100}
	got := resolveConfig(cfg, nil, "")
	if got.MaxTokens != 100 {
		t.Errorf("无 profile 时应保持 MaxTokens=100, got %d", got.MaxTokens)
	}
	if got.Key != "sk-1" {
		t.Errorf("无 profile 时应保持 Key=%q", got.Key)
	}
}

// TestResolveConfig_NilProfiles 验证 profiles map 为 nil 时原样返回。
func TestResolveConfig_NilProfiles(t *testing.T) {
	cfg := KeyConfig{Profile: "night"}
	got := resolveConfig(cfg, nil, "default")
	// profile 找不到 → 原样
	if got.MaxTokens != 0 {
		t.Errorf("profiles=nil 应原样返回, got MaxTokens=%d", got.MaxTokens)
	}
}

// TestResolveConfig_DefaultProfile 验证没有显式 profile 时自动用 default。
func TestResolveConfig_DefaultProfile(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1"}
	profiles := map[string]InterceptorProfile{
		"default": {MaxTokens: 10000, Window: "24h"},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 10000 {
		t.Errorf("default profile 应合并 MaxTokens=10000, got %d", got.MaxTokens)
	}
	if got.Window != "24h" {
		t.Errorf("default profile 应合并 Window=24h, got %s", got.Window)
	}
	if got.Key != "sk-1" {
		t.Errorf("Key 应保持, got %q", got.Key)
	}
}

// TestResolveConfig_ExplicitProfile 验证 profile 引用合并。
func TestResolveConfig_ExplicitProfile(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1", Profile: "night_block"}
	profiles := map[string]InterceptorProfile{
		"night_block": {
			MaxTokens: 5000000,
			MaxReqs:   2000,
			Window:    "12h",
			TimeBlock: &TimeBlock{Start: "22:00", End: "08:00"},
		},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 5000000 {
		t.Errorf("night_block 应合并 MaxTokens=5000000, got %d", got.MaxTokens)
	}
	if got.MaxReqs != 2000 {
		t.Errorf("night_block 应合并 MaxReqs=2000, got %d", got.MaxReqs)
	}
	if got.Window != "12h" {
		t.Errorf("night_block 应合并 Window=12h, got %s", got.Window)
	}
	if got.TimeBlock == nil || got.TimeBlock.Start != "22:00" {
		t.Errorf("night_block 应合并 TimeBlock, got %+v", got.TimeBlock)
	}
}

// TestResolveConfig_Override 验证 override 覆盖 profile。
func TestResolveConfig_Override(t *testing.T) {
	cfg := KeyConfig{
		Key:     "sk-1",
		Profile: "night_block",
		Override: &InterceptorProfile{
			MaxTokens: 2000000, // 覆盖 profile 的 5000000
		},
	}
	profiles := map[string]InterceptorProfile{
		"night_block": {MaxTokens: 5000000, Window: "12h"},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 2000000 {
		t.Errorf("override 应覆盖 MaxTokens=2000000, got %d", got.MaxTokens)
	}
	if got.Window != "12h" {
		t.Errorf("未 override 的 Window 应保留 profile 的 12h, got %s", got.Window)
	}
}

// TestResolveConfig_AliasDirectOverridesProfile 验证 alias 直接字段覆盖 profile。
func TestResolveConfig_AliasDirectOverridesProfile(t *testing.T) {
	cfg := KeyConfig{
		Key:       "sk-1",
		Profile:   "night_block",
		MaxTokens: 999, // 直接写在 alias 里的,优先级最高
	}
	profiles := map[string]InterceptorProfile{
		"night_block": {MaxTokens: 5000000},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 999 {
		t.Errorf("alias 直接字段(999)应覆盖 profile(5000000), got %d", got.MaxTokens)
	}
}

// TestResolveConfig_AliasDirectOverridesOverride 验证 alias 直接字段 > override > profile。
func TestResolveConfig_AliasDirectOverridesOverride(t *testing.T) {
	cfg := KeyConfig{
		Key:       "sk-1",
		Profile:   "night_block",
		MaxTokens: 777,
		Override: &InterceptorProfile{
			MaxTokens: 888,
		},
	}
	profiles := map[string]InterceptorProfile{
		"night_block": {MaxTokens: 5000000},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 777 {
		t.Errorf("alias 直接字段(777) > override(888) > profile(5000000), got %d", got.MaxTokens)
	}
}

// TestResolveConfig_ZeroValues 验证 0 值不覆盖。
func TestResolveConfig_ZeroValues(t *testing.T) {
	// alias 有 MaxReqs=10, profile 有 MaxTokens=1000 和 MaxReqs=0
	// → MaxReqs 保持 10(profile 的 0 不覆盖),MaxTokens 合并
	cfg := KeyConfig{MaxReqs: 10, Profile: "rate_profile"}
	profiles := map[string]InterceptorProfile{
		"rate_profile": {MaxTokens: 1000, MaxReqs: 0},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 1000 {
		t.Errorf("MaxTokens 应合并 1000, got %d", got.MaxTokens)
	}
	if got.MaxReqs != 10 {
		t.Errorf("MaxReqs 应保持 alias 的 10, profile 的 0 不应覆盖, got %d", got.MaxReqs)
	}
}

// TestResolveConfig_ProfileNotFound 验证 profile 找不到时原样返回。
func TestResolveConfig_ProfileNotFound(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1", Profile: "nonexistent", MaxTokens: 42}
	profiles := map[string]InterceptorProfile{
		"night_block": {MaxTokens: 5000},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 42 {
		t.Errorf("profile 找不到应保持原值 42, got %d", got.MaxTokens)
	}
}

// TestResolveConfig_EmptyProfileID 验证 Profile="" 时不用任何 profile。
func TestResolveConfig_EmptyProfileID(t *testing.T) {
	// Profile="" 且 defaultProfile="default" → 自动用 default
	cfg := KeyConfig{Key: "sk-1", Profile: "", MaxTokens: 42}
	profiles := map[string]InterceptorProfile{
		"default": {MaxTokens: 1000},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 42 {
		t.Errorf("空的显式 Profile 等效于未设置,应合并 default(1000) 但 alias 直接字段(42)优先级更高, got %d", got.MaxTokens)
	}
}

// TestResolveConfig_DefaultProfileNoAliasFields 验证只有 default 且 alias 没写字段时正确合并。
func TestResolveConfig_DefaultProfileNoAliasFields(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1"} // 没有 MaxTokens
	profiles := map[string]InterceptorProfile{
		"default": {MaxTokens: 10000, Window: "24h"},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 10000 {
		t.Errorf("default profile 应合并 MaxTokens=10000, got %d", got.MaxTokens)
	}
	if got.Window != "24h" {
		t.Errorf("default profile 应合并 Window=24h, got %s", got.Window)
	}
}

// TestResolveConfig_RateBurst 验证 rate/burst 也被合并。
func TestResolveConfig_RateBurst(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1", Profile: "limited"}
	profiles := map[string]InterceptorProfile{
		"limited": {Rate: 60, Burst: 10},
	}
	got := resolveConfig(cfg, profiles, "")
	if got.Rate != 60 {
		t.Errorf("profile 应合并 Rate=60, got %d", got.Rate)
	}
	if got.Burst != 10 {
		t.Errorf("profile 应合并 Burst=10, got %d", got.Burst)
	}
}

// TestResolveConfig_Expires 验证 expires 被合并。
func TestResolveConfig_Expires(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1", Profile: "limited"}
	profiles := map[string]InterceptorProfile{
		"limited": {Expires: "2026-12-31"},
	}
	got := resolveConfig(cfg, profiles, "")
	if got.Expires != "2026-12-31" {
		t.Errorf("profile 应合并 Expires=2026-12-31, got %s", got.Expires)
	}
}

// TestResolveConfig_KeyHeaderNotOverwritten 验证 Key/Header/Prefix 不被 profile 覆盖。
func TestResolveConfig_KeyHeaderNotOverwritten(t *testing.T) {
	cfg := KeyConfig{Key: "my-key", Header: "x-api-key", Prefix: "MyPrefix "}
	profiles := map[string]InterceptorProfile{
		"default": {Rate: 60},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.Key != "my-key" {
		t.Errorf("Key 不应被覆盖, got %q", got.Key)
	}
	if got.Header != "x-api-key" {
		t.Errorf("Header 不应被覆盖, got %q", got.Header)
	}
	if got.Prefix != "MyPrefix " {
		t.Errorf("Prefix 不应被覆盖, got %q", got.Prefix)
	}
}

// TestResolveConfig_NonZeroProfileFieldOverridesAliasZero 验证 profile 的非零字段覆盖 alias 的零值。
func TestResolveConfig_NonZeroProfileFieldOverridesAliasZero(t *testing.T) {
	cfg := KeyConfig{Key: "sk-1"}
	profiles := map[string]InterceptorProfile{
		"default": {MaxTokens: 1000, Window: "24h"},
	}
	got := resolveConfig(cfg, profiles, "default")
	if got.MaxTokens != 1000 {
		t.Errorf("alias 的 MaxTokens 为 0(=未设置), profile 的 1000 应合并, got %d", got.MaxTokens)
	}
}
