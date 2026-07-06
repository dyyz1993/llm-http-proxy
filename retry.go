// retry.go — 上游请求重试配置和逻辑
//
// 在 keys.yaml 的 retry 段配置。全局生效。
// 当上游返回特定状态码或连接失败时，自动重试。
// 重试耗尽后返回 fallback_status 状态码（默认 429）给客户端。

package main

import (
	"fmt"
	"log"
	"math"
	"time"
)

// RetryConfig 上游请求重试配置。零值 = 使用默认值。
type RetryConfig struct {
	MaxAttempts       int   `yaml:"max_attempts"`       // 最大尝试次数(含首次, 1=不重试)
	RetryOnCodes      []int `yaml:"retry_on_codes"`     // 上游返回这些码时触发重试
	RetryOnError      bool  `yaml:"retry_on_error"`     // 连接错误(DNS/连接拒绝等)也重试
	BackoffMs         int   `yaml:"backoff"`            // 退避基准毫秒
	BackoffMultiplier int   `yaml:"backoff_multiplier"` // 退避倍数(每次 attempt × 这个倍数)
	FallbackStatus    int   `yaml:"fallback_status"`    // 重试耗尽后返回给客户端的状态码
}

// defaultRetryConfig 返回默认重试配置。
func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:       3,
		RetryOnCodes:      []int{429, 500, 502, 503},
		RetryOnError:      true,
		BackoffMs:         500,
		BackoffMultiplier: 2,
		FallbackStatus:    429,
	}
}

// effective 返回有效的配置(零值字段用默认值填充)。
// 注意:完全零值(未配置)时 MaxAttempts=1(不重试)。
func (r RetryConfig) effective() RetryConfig {
	d := defaultRetryConfig()
	// 完全零值:未配置,返回不重试
	if r.MaxAttempts == 0 && len(r.RetryOnCodes) == 0 && !r.RetryOnError &&
		r.FallbackStatus == 0 && r.BackoffMs == 0 && r.BackoffMultiplier == 0 {
		return RetryConfig{MaxAttempts: 1}
	}
	if r.MaxAttempts < 1 {
		r.MaxAttempts = d.MaxAttempts
	}
	if len(r.RetryOnCodes) == 0 {
		r.RetryOnCodes = d.RetryOnCodes
	}
	if !r.RetryOnError {
		r.RetryOnError = d.RetryOnError
	}
	if r.BackoffMs < 1 {
		r.BackoffMs = d.BackoffMs
	}
	if r.BackoffMultiplier < 1 {
		r.BackoffMultiplier = d.BackoffMultiplier
	}
	if r.FallbackStatus < 100 {
		r.FallbackStatus = d.FallbackStatus
	}
	return r
}

// shouldRetryCode 检查是否应该重试给定的 HTTP 状态码。
// 只在非重试尝试(attempt > 0)时才返回 true。
func (r RetryConfig) shouldRetryCode(code int) bool {
	cfg := r.effective()
	for _, c := range cfg.RetryOnCodes {
		if code == c {
			return true
		}
	}
	return false
}

// backoffDuration 计算第 attempt 次(从0开始)重试的等待时间。
func (r RetryConfig) backoffDuration(attempt int) time.Duration {
	cfg := r.effective()
	// 首次 attempt=0 → backoffMs × 1
	// 二次 attempt=1 → backoffMs × multiplier
	// 三次 attempt=2 → backoffMs × multiplier²
	ms := float64(cfg.BackoffMs) * math.Pow(float64(cfg.BackoffMultiplier), float64(attempt))
	d := time.Duration(ms) * time.Millisecond
	if d > 30*time.Second {
		d = 30 * time.Second // 上限 30 秒
	}
	return d
}

// retryLabel 返回重试进度的描述文字。
func (r RetryConfig) retryLabel(attempt, maxAttempts int) string {
	return fmt.Sprintf("第%d/%d次重试", attempt+1, maxAttempts)
}

// logRetry 记录一条重试日志。
func logRetry(alias, model string, attempt, maxAttempts int, status int, err error) {
	msg := fmt.Sprintf("上游重试: alias=%s", alias)
	if model != "" {
		msg += fmt.Sprintf(" model=%q", model)
	}
	msg += fmt.Sprintf(" 第%d/%d次 上次状态=%d", attempt+1, maxAttempts, status)
	if err != nil {
		msg += fmt.Sprintf(" err=%v", err)
	}
	log.Print(msg)
}
