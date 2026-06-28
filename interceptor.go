package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 统一拦截器链架构
//
// handleKeyRoute 中原本是一串手写 if-return 检查,每个检查有自己的参数、状态码、
// 消息格式、header 设置。新增检查就要在中间插一段 if,容易漏 return 或顺序搞错。
//
// 现在改为拦截器链(Chain of Responsibility):
//
//	请求 → checkTimeBlock → checkQuota → checkRateLimit → checkDomainWhitelist → checkSetup → 转发
//	            ↓              ↓             ↓                  ↓
//	         403(拒绝)      402(拒绝)      429(拒绝)          403(拒绝)
//
// checkSetup 不会拒绝请求,它只负责收集全局规则和注入 header 信息到 ctx.
// ---------------------------------------------------------------------------

// CheckResult 拦截结果。
type CheckResult struct {
	Blocked bool        // true=已拦截(请求终止),false=放行
	Status  int         // HTTP 状态码(仅 Blocked=true)
	Message string      // 错误消息(仅 Blocked=true)
	Headers http.Header // 附加响应头(如 Retry-After,仅 Blocked=true)
}

// CheckFunc 拦截器函数。
// ctx 承载请求上下文和依赖服务,拦截器可读取和修改 ctx。
type CheckFunc func(ctx *CheckContext) *CheckResult

// CheckContext 请求拦截上下文。
type CheckContext struct {
	// 输入(由 handleKeyRoute 解析后设置)
	Alias   string
	Target  string
	Domain  string
	Config  KeyConfig
	Request *http.Request

	// 依赖服务(全部注入,拦截器按需取用)
	Store    *keyStore
	Usage    *usageStats
	Settings *settingsManager

	// 输出(由 checkSetup 填写)
	StatLabel        string
	HeadersToInject  http.Header
	ImageFilter      []ImageFilterRule
	TokenMultipliers []TokenMultiplierRule
}

// keyRouteChecks 是 key 注入模式(k/{alias}/...)的拦截器链。
// 顺序说明:
//
//  1. checkTimeBlock       — 禁止时段
//  2. checkQuota           — 用量超限(含 Retry-After)
//  3. checkRateLimit       — 请求频率超限(含 Retry-After)
//  4. checkDomainWhitelist — 域名白名单
//  5. checkSetup           — 收集全局规则+注入 header(永不拒绝)
var keyRouteChecks = []CheckFunc{
	checkTimeBlock,
	checkQuota,
	checkRateLimit,
	checkDomainWhitelist,
	checkSetup,
}

// passthroughChecks 是透传模式的拦截器链(仅有 checkSetup)。
// 透传模式不做任何审查(客户端自带 key,域名不受限)。
var passthroughChecks = []CheckFunc{
	checkSetupPassthrough,
}

// runChecks 依次执行拦截器,遇到拦截则写入响应并返回 false。
// 所有拦截器通过后返回 true。
func runChecks(w http.ResponseWriter, checks []CheckFunc, ctx *CheckContext) bool {
	for i, check := range checks {
		result := check(ctx)
		if result.Blocked {
			for k, vals := range result.Headers {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			http.Error(w, result.Message+"\n", result.Status)
			return false
		}
		_ = i
	}
	return true
}

// ---------------------------------------------------------------------------
// 拦截器实现
// ---------------------------------------------------------------------------

// checkTimeBlock 检查禁止时段(北京时间,每天重复)。
// 匹配 → 403 Forbidden
func checkTimeBlock(ctx *CheckContext) *CheckResult {
	if ctx.Config.TimeBlock != nil && ctx.Config.TimeBlock.IsBlocked(time.Now()) {
		log.Printf("禁止时段: alias=%s start=%s end=%s",
			ctx.Alias, ctx.Config.TimeBlock.Start, ctx.Config.TimeBlock.End)
		return &CheckResult{
			Blocked: true,
			Status:  http.StatusForbidden,
			Message: "该 key 在当前时段禁止访问 (alias: " + ctx.Alias + ")",
		}
	}
	return &CheckResult{}
}

// checkQuota 检查用量限额(窗口内 token/请求次数)。
// 超限 → 402 Payment Required + Retry-After
func checkQuota(ctx *CheckContext) *CheckResult {
	if ctx.Usage != nil && ctx.Config.HasQuota() {
		ok, reason, retryAfter := ctx.Usage.checkQuota(ctx.Alias, ctx.Config)
		if !ok {
			h := http.Header{}
			if retryAfter > 0 {
				h.Set("Retry-After", fmt.Sprintf("%.0f", retryAfter.Seconds()))
			}
			return &CheckResult{
				Blocked: true,
				Status:  http.StatusPaymentRequired,
				Message: reason,
				Headers: h,
			}
		}
	}
	return &CheckResult{}
}

// checkRateLimit 检查请求频率(token bucket)。
// 超限 → 429 Too Many Requests + Retry-After: 60
func checkRateLimit(ctx *CheckContext) *CheckResult {
	if !ctx.Store.allow(ctx.Alias) {
		h := http.Header{}
		h.Set("Retry-After", "60")
		return &CheckResult{
			Blocked: true,
			Status:  http.StatusTooManyRequests,
			Message: "请求过于频繁,请稍后重试 (alias: " + ctx.Alias + ")",
			Headers: h,
		}
	}
	return &CheckResult{}
}

// checkDomainWhitelist 检查目标域名是否在白名单内。
// 不在 → 403 Forbidden
func checkDomainWhitelist(ctx *CheckContext) *CheckResult {
	if !ctx.Settings.IsAllowed(ctx.Domain) {
		log.Printf("拒绝代理: 目标域名 %q 不在白名单 (alias=%s)", ctx.Domain, ctx.Alias)
		return &CheckResult{
			Blocked: true,
			Status:  http.StatusForbidden,
			Message: "目标域名不在白名单: " + ctx.Domain,
		}
	}
	return &CheckResult{}
}

// checkSetup 收集全局规则和注入 header(永不拒绝)。
// 必须放在 key 注入模式拦截器链的最后。
func checkSetup(ctx *CheckContext) *CheckResult {
	inject := pickHeader(ctx.Config, ctx.Request.Header)
	if len(inject) == 0 {
		// 客户端没带 x-api-key 也没带 Authorization → 不注入,直接透传
		// (上游会用客户端自己的 header,通常没 key 会被拒)
	}
	ctx.HeadersToInject = inject
	ctx.StatLabel = "key:" + ctx.Alias
	ctx.ImageFilter = ctx.Store.getImageFilter()
	ctx.TokenMultipliers = ctx.Store.getTokenMultipliers()
	return &CheckResult{}
}

// checkSetupPassthrough 透传模式的收集器(永不拒绝)。
func checkSetupPassthrough(ctx *CheckContext) *CheckResult {
	if ctx.Store != nil {
		ctx.ImageFilter = ctx.Store.getImageFilter()
		ctx.TokenMultipliers = ctx.Store.getTokenMultipliers()
	}
	return &CheckResult{}
}
