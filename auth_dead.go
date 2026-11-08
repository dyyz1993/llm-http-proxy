// auth_dead.go — key 认证失效自动隔离
//
// 背景(2026-11-08 排查): ~12 个死 key 每天 401 约 1.4 万次(配额轮询每 5 分钟
// 无差别打全部 key)。401 与 429/502 不同——429/502 是"暂时不行",401 是
// "权限没了",冷却重试纯属浪费。
//
// 规则: 上游返回 401 且响应体为 z.ai 身份验证失败(code=1000)时计数;
// 连续 authDeadThreshold 次 → 标记 AuthDead 并持久化:
//   - group 选成员跳过
//   - 配额轮询跳过(隔离满 24h 才低频探测一次,成功自动复活)
//   - 管理界面显示"认证失效"徽标 + 复活按钮(换新 key 后手动恢复)
// 任何 2xx 成功响应会清除计数;若该 key 处于隔离中且突然成功(如账号恢复),
// 自动复活并记录日志。

package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// authDeadThreshold 连续多少次"身份验证失败"才隔离。
// 2 次 = 跨两个轮询周期,防单次网络异常导致的偶发 401 误杀好 key。
const authDeadThreshold = 2

// authDeadReprobeInterval 隔离期间低频探测间隔(到期自动探测,成功自动复活)。
const authDeadReprobeInterval = 24 * time.Hour

// authFailureCode z.ai 身份验证失败的错误码(响应体 {"error":{"code":"1000",...}})。
const authFailureCode = "1000"

// isAuthVerificationFailure 判断 401 响应体是否为"身份验证失败"。
// 只认 z.ai 的 code=1000(或消息含"身份验证");其他语义的 401(如欠费类)
// 不参与隔离计数,避免误杀——那些是"充钱能活"的错误,走长冷却即可。
func isAuthVerificationFailure(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 截前 2KB 足够(错误体很小;groupWriter.interceptBody 只有 256 字节也够)
	if len(body) > 2048 {
		body = body[:2048]
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Code == authFailureCode {
			return true
		}
		if payload.Error.Code == "" && strings.Contains(payload.Error.Message, "身份验证") {
			return true
		}
		return false
	}
	// 非 JSON(极端情况):退化为子串匹配
	return strings.Contains(string(body), "身份验证")
}

// markAuth401 上游返回 401 时调用(转发路径,含配额轮询的自代理请求)。
// body 为响应体(用于识别"身份验证失败"语义);非该语义的 401 不计数。
func markAuth401(ks *keyStore, alias string, body []byte) {
	if ks == nil || alias == "" || alias == "-" {
		return
	}
	if !isAuthVerificationFailure(body) {
		return
	}
	countAuthFailure(ks, alias)
}

// markAuth401Msg 配额轮询直连接口(qc.fetchOne,不经过转发路径)时调用:
// 用响应 msg 判定"身份验证失败"语义并计数。转发路径钩子看不到直连请求,
// 没有这条通道死 key 永远不会被隔离(2026-11-08 v2.5.71 部署后发现的盲区)。
func markAuth401Msg(ks *keyStore, alias, msg string) {
	if ks == nil || alias == "" || !strings.Contains(msg, "身份验证") {
		return
	}
	countAuthFailure(ks, alias)
}

// countAuthFailure 计数并按阈值隔离(所有 401 计数入口的公共尾部)。
func countAuthFailure(ks *keyStore, alias string) {
	ks.mu.Lock()
	ks.auth401Count[alias]++
	n := ks.auth401Count[alias]
	ks.mu.Unlock()
	if n < authDeadThreshold {
		return
	}

	cfg, ok := ks.lookup(alias)
	if !ok || cfg.AuthDead {
		return // 已隔离,不重复写
	}
	cfg.AuthDead = true
	cfg.AuthDeadAt = time.Now().In(beijing).Format("2006-01-02 15:04:05")
	if err := ks.setConfig(alias, cfg); err != nil {
		log.Printf("认证失效自动隔离失败(持久化错误): %s: %v", alias, err)
		return
	}
	log.Printf("alias 连续 %d 次 401 身份验证失败,自动隔离: %s (group/轮询跳过;管理界面可复活)",
		n, alias)
}

// noteAuthSuccess 收到 2xx 成功响应时调用:清除 401 计数;
// 若 key 处于隔离中却成功了(如账号恢复),自动复活。
func noteAuthSuccess(ks *keyStore, alias string) {
	if ks == nil || alias == "" || alias == "-" {
		return
	}
	ks.mu.Lock()
	if ks.auth401Count[alias] > 0 {
		ks.auth401Count[alias] = 0
	}
	ks.mu.Unlock()

	cfg, ok := ks.lookup(alias)
	if !ok || !cfg.AuthDead {
		return
	}
	cfg.AuthDead = false
	cfg.AuthDeadAt = ""
	if err := ks.setConfig(alias, cfg); err != nil {
		log.Printf("认证失效自动复活失败(持久化错误): %s: %v", alias, err)
		return
	}
	log.Printf("alias 隔离期间收到成功响应,自动复活: %s", alias)
}

// clearAuthDead 手动复活(管理界面按钮):清标记、清时间、清计数。
func clearAuthDead(ks *keyStore, alias string) error {
	if ks == nil {
		return nil
	}
	cfg, ok := ks.lookup(alias)
	if !ok {
		return nil
	}
	ks.mu.Lock()
	ks.auth401Count[alias] = 0
	ks.mu.Unlock()
	if !cfg.AuthDead && cfg.AuthDeadAt == "" {
		return nil // 本来就没隔离
	}
	cfg.AuthDead = false
	cfg.AuthDeadAt = ""
	if err := ks.setConfig(alias, cfg); err != nil {
		return err
	}
	log.Printf("alias 认证失效已手动复活: %s", alias)
	return nil
}

// shouldProbeAuthDead 隔离中的 key 是否该低频探测了(距隔离/上次探测满 24h)。
func shouldProbeAuthDead(cfg KeyConfig, now time.Time) bool {
	if !cfg.AuthDead {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", cfg.AuthDeadAt, beijing)
	if err != nil {
		return true // 时间解析失败(手工编辑过的配置),探一次
	}
	return now.Sub(t) >= authDeadReprobeInterval
}

// touchAuthDead 刷新隔离时刻(探测仍失败时调用,让下一个 24h 周期从现在起算)。
func touchAuthDead(ks *keyStore, alias string) {
	cfg, ok := ks.lookup(alias)
	if !ok || !cfg.AuthDead {
		return
	}
	cfg.AuthDeadAt = time.Now().In(beijing).Format("2006-01-02 15:04:05")
	if err := ks.setConfig(alias, cfg); err != nil {
		log.Printf("认证失效探测时刻刷新失败(持久化错误): %s: %v", alias, err)
	}
}
