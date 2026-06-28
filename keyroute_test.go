package main

import (
	"testing"
	"time"
)

// TestHasQuota 验证 HasQuota 的各种组合。
func TestHasQuota(t *testing.T) {
	tests := []struct {
		name string
		cfg  KeyConfig
		want bool
	}{
		{"全零", KeyConfig{}, false},
		{"只有 MaxReqs", KeyConfig{MaxReqs: 5}, true},
		{"只有 MaxTokens", KeyConfig{MaxTokens: 1000}, true},
		{"两者都有", KeyConfig{MaxReqs: 5, MaxTokens: 1000}, true},
		{"负数 MaxReqs", KeyConfig{MaxReqs: -1}, false},
		{"负数 MaxTokens", KeyConfig{MaxTokens: -1}, false},
		{"零值", KeyConfig{MaxReqs: 0, MaxTokens: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.HasQuota(); got != tt.want {
				t.Errorf("HasQuota() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseWindowDurationEdgeCases 验证 parseWindowDuration 的边界情况。
func TestParseWindowDurationEdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"  5h", 5 * time.Hour, false},              // 带空格
		{"5h  ", 5 * time.Hour, false},              // 尾空格
		{"  5h  ", 5 * time.Hour, false},            // 双向空格
		{"0h", 0, false},                            // 0 小时
		{"0d", 0, false},                            // 0 天
		{"100000d", 100000 * 24 * time.Hour, false}, // 超大
		{"abc", 0, true},
		{"10m", 10 * time.Minute, false},   // Go time.ParseDuration 分钟
		{"10s", 10 * time.Second, false},   // Go time.ParseDuration 秒
		{"1h30m", 90 * time.Minute, false}, // Go time.ParseDuration 组合
		{"-", 0, true},                     // 只有负号
		{"d", 0, true},                     // 只有 d
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseWindowDuration(tt.input)
			if tt.err {
				if err == nil {
					t.Errorf("parseWindowDuration(%q) 应报错, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseWindowDuration(%q) 不应报错: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseWindowDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestKeyStoreAllow_Direct 直接测试 token bucket 放行/限流逻辑。
func TestKeyStoreAllow_Direct(t *testing.T) {
	ks := newKeyStore()
	// Rate=120/min = 2/sec, Burst=2
	ks.configs["limited"] = KeyConfig{Key: "sk-1", Rate: 120, Burst: 2}
	ks.mu.Lock()
	ks.getOrCreateLimiter("limited", ks.configs["limited"])
	ks.mu.Unlock()

	// burst=2,前 2 次应允许
	if !ks.allow("limited") {
		t.Error("第 1 次应允许(burst=2)")
	}
	if !ks.allow("limited") {
		t.Error("第 2 次应允许(burst=2)")
	}
	// 第 3 次应被限(已用完 burst,来不及补充)
	if ks.allow("limited") {
		t.Error("第 3 次应被限流(burst=2,已用完)")
	}
}

// TestKeyStoreAllow_NoLimit 验证无限流时 always allow。
func TestKeyStoreAllow_NoLimit(t *testing.T) {
	ks := newKeyStore()
	ks.configs["nolimit"] = KeyConfig{Key: "sk-1"}
	// 无限流 → 永远允许
	for i := 0; i < 100; i++ {
		if !ks.allow("nolimit") {
			t.Errorf("第 %d 次: 无限流应永远允许", i)
		}
	}
}

// TestKeyStoreAllow_UnknownAlias 验证未知 alias 也允许(防御性)。
func TestKeyStoreAllow_UnknownAlias(t *testing.T) {
	ks := newKeyStore()
	if !ks.allow("nonexistent") {
		t.Error("未知 alias 应允许(无限流)")
	}
}

// TestKeyStoreAllow_Refill 验证令牌桶随时间补充。
func TestKeyStoreAllow_Refill(t *testing.T) {
	ks := newKeyStore()
	// Rate=60/min = 1/sec, Burst=1
	ks.configs["slow"] = KeyConfig{Key: "sk-1", Rate: 60, Burst: 1}
	ks.mu.Lock()
	ks.getOrCreateLimiter("slow", ks.configs["slow"])
	ks.mu.Unlock()

	// 第 1 次允许(burst=1)
	if !ks.allow("slow") {
		t.Fatal("第 1 次应允许")
	}

	// 第 2 次应被限
	if ks.allow("slow") {
		t.Fatal("第 2 次应被限(burst=1)")
	}

	// 等 1 秒(refill 1 个 token)
	time.Sleep(1100 * time.Millisecond)

	// 应有一个补充的 token
	if !ks.allow("slow") {
		t.Error("等待 1 秒后应补充 1 个 token,应允许")
	}
}

// --- 禁止时段测试 ---------------------------------------------------------

func TestTimeBlock_Nil(t *testing.T) {
	var tb *TimeBlock
	if tb.IsBlocked(time.Now()) {
		t.Error("nil TimeBlock 不应阻止")
	}
}

func TestTimeBlock_Empty(t *testing.T) {
	tb := &TimeBlock{}
	if tb.IsBlocked(time.Now()) {
		t.Error("空 TimeBlock 不应阻止")
	}
}

func TestTimeBlock_InvalidFormat(t *testing.T) {
	tb := &TimeBlock{Start: "abc", End: "def"}
	if tb.IsBlocked(time.Now()) {
		t.Error("非法格式不应阻止")
	}
}

func TestTimeBlock_WrapMidnight_Blocked(t *testing.T) {
	// 22:00-08:00 跨日区间, 23:30 应被阻止
	tb := &TimeBlock{Start: "22:00", End: "08:00"}
	now := time.Date(2026, 6, 28, 23, 30, 0, 0, beijing)
	if !tb.IsBlocked(now) {
		t.Error("23:30 北京时间应在 22:00-08:00 区间内被阻止")
	}
}

func TestTimeBlock_WrapMidnight_Allowed(t *testing.T) {
	// 22:00-08:00 跨日区间, 21:00 应允许
	tb := &TimeBlock{Start: "22:00", End: "08:00"}
	now := time.Date(2026, 6, 28, 21, 0, 0, 0, beijing)
	if tb.IsBlocked(now) {
		t.Error("21:00 北京时间应在 22:00-08:00 区间外被允许")
	}
}

func TestTimeBlock_WrapMidnight_BoundaryEnd(t *testing.T) {
	// 22:00-08:00 跨日区间, 08:00 应允许(左闭右开)
	tb := &TimeBlock{Start: "22:00", End: "08:00"}
	now := time.Date(2026, 6, 28, 8, 0, 0, 0, beijing)
	if tb.IsBlocked(now) {
		t.Error("08:00 是边界终点应允许")
	}
}

func TestTimeBlock_WrapMidnight_BoundaryBeforeEnd(t *testing.T) {
	// 22:00-08:00 跨日区间, 07:59 应被阻止
	tb := &TimeBlock{Start: "22:00", End: "08:00"}
	now := time.Date(2026, 6, 28, 7, 59, 0, 0, beijing)
	if !tb.IsBlocked(now) {
		t.Error("07:59 在 22:00-08:00 区间内应阻止")
	}
}

func TestTimeBlock_SameDay_Blocked(t *testing.T) {
	// 09:00-18:00 单日区间, 14:00 应被阻止
	tb := &TimeBlock{Start: "09:00", End: "18:00"}
	now := time.Date(2026, 6, 28, 14, 0, 0, 0, beijing)
	if !tb.IsBlocked(now) {
		t.Error("14:00 应在 09:00-18:00 区间内被阻止")
	}
}

func TestTimeBlock_SameDay_Allowed(t *testing.T) {
	// 09:00-18:00 单日区间, 08:59 应允许
	tb := &TimeBlock{Start: "09:00", End: "18:00"}
	now := time.Date(2026, 6, 28, 8, 59, 0, 0, beijing)
	if tb.IsBlocked(now) {
		t.Error("08:59 应在 09:00-18:00 区间外被允许")
	}
}

func TestTimeBlock_SameDay_BoundaryEnd(t *testing.T) {
	// 09:00-18:00 单日区间, 18:00 应允许(左闭右开)
	tb := &TimeBlock{Start: "09:00", End: "18:00"}
	now := time.Date(2026, 6, 28, 18, 0, 0, 0, beijing)
	if tb.IsBlocked(now) {
		t.Error("18:00 是边界终点应允许")
	}
}

func TestTimeBlock_AllDay(t *testing.T) {
	// 00:00-00:00 = 全天禁止
	tb := &TimeBlock{Start: "00:00", End: "00:00"}
	if !tb.IsBlocked(time.Date(2026, 6, 28, 3, 0, 0, 0, beijing)) {
		t.Error("全天禁止: 03:00 应阻止")
	}
	if !tb.IsBlocked(time.Date(2026, 6, 28, 15, 0, 0, 0, beijing)) {
		t.Error("全天禁止: 15:00 应阻止")
	}
}

func TestTimeBlock_UTCconversion(t *testing.T) {
	// 服务器时间是 UTC,北京时间 = UTC+8,验证时区转换
	// UTC 14:00 = 北京时间 22:00,应在 22:00-08:00 区间内
	tb := &TimeBlock{Start: "22:00", End: "08:00"}
	utc := time.Date(2026, 6, 28, 14, 0, 0, 0, time.UTC)
	if !tb.IsBlocked(utc) {
		t.Error("UTC 14:00 = 北京时间 22:00,应在禁止区间内")
	}
	// UTC 12:00 = 北京时间 20:00,应在区间外
	utc2 := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	if tb.IsBlocked(utc2) {
		t.Error("UTC 12:00 = 北京时间 20:00,应在禁止区间外")
	}
}
