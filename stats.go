// 请求来源统计:采集访问 IP 和掩码后的鉴权 key,只统计不泄露。
//
// 隐私原则:
//   - 不记录请求 body / 路径 / query / 完整 header
//   - IP 直接记录(来源统计必需),key 必须掩码
//   - key 取不到记为 "-"
//
// key 提取(按常见大模型 API 约定):
//   - Authorization: Bearer <key>   (OpenAI / GLM / 多数兼容 API)
//   - x-api-key: <key>              (Anthropic Claude)
//   - api-key: <key>                (Azure OpenAI 等)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// keyEntry 是单个 (IP, 掩码key) 的累计统计。
type keyEntry struct {
	Count       int64     `json:"count"`
	LastSeen    time.Time `json:"last_seen"`
	LastStatus  int       `json:"last_status"`
	LastTarget  string    `json:"last_target"` // 只记 host,不记 path
}

// ipStats 是某个 IP 下的若干 key 的统计。
type ipStats struct {
	Keys map[string]*keyEntry // key = 掩码后的 key
}

// statsCollector 是全局统计收集器,线程安全。
type statsCollector struct {
	mu   sync.Mutex
	data map[string]*ipStats // key = IP
}

func newStatsCollector() *statsCollector {
	return &statsCollector{data: make(map[string]*ipStats)}
}

// record 记录一次请求。target 只传 host,避免记录路径。
func (s *statsCollector) record(ip, maskedKey, targetHost string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	is, ok := s.data[ip]
	if !ok {
		is = &ipStats{Keys: make(map[string]*keyEntry)}
		s.data[ip] = is
	}
	ke, ok := is.Keys[maskedKey]
	if !ok {
		ke = &keyEntry{}
		is.Keys[maskedKey] = ke
	}
	ke.Count++
	ke.LastSeen = time.Now()
	ke.LastStatus = status
	ke.LastTarget = targetHost
}

// snapshot 返回当前统计的快照(深拷贝,避免持锁渲染 JSON)。
func (s *statsCollector) snapshot() map[string]*ipStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*ipStats, len(s.data))
	for ip, is := range s.data {
		is2 := &ipStats{Keys: make(map[string]*keyEntry, len(is.Keys))}
		for k, ke := range is.Keys {
			ke2 := *ke
			is2.Keys[k] = &ke2
		}
		out[ip] = is2
	}
	return out
}

// statsHandler 处理 GET /__stats,返回统计汇总。
//
// 查询参数:
//
//	by=ip    (默认)按来源 IP 聚合,看每个 IP 用了哪些 key
//	by=key   按 key 聚合,看每个 key 触发了哪些 IP(反向查询)
//	format=json (默认)返回 JSON
//	format=table 返回 ASCII 表格(人读友好)
func statsHandler(s *statsCollector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		by := r.URL.Query().Get("by")
		if by != "key" {
			by = "ip" // 默认
		}
		format := r.URL.Query().Get("format")
		if format != "table" {
			format = "json" // 默认
		}

		snap := s.snapshot()

		if format == "table" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			renderStatsTable(w, snap, by)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if by == "key" {
			// 反向视图:以 key 为顶层
			json.NewEncoder(w).Encode(statsByKey(snap))
		} else {
			json.NewEncoder(w).Encode(statsByIP(snap))
		}
	}
}

// rowView 是表格/列表里的一行:一个 (维度值, 对端, 统计) 三元组。
type rowView struct {
	Primary string // 维度值(IP 或 key)
	Peer    string // 对端(key 或 IP)
	Count   int64
	LastSeen    time.Time
	LastStatus  int
	LastTarget  string
}

// statsByIP 生成按 IP 聚合的 JSON 视图,带去重计数。
type ipAggView struct {
	Keys       map[string]*keyEntry `json:"keys"`
	DistinctKeys int                `json:"distinct_keys"` // 该 IP 用了多少个不同 key
	TotalCount  int64              `json:"total_count"`    // 该 IP 总调用数
}

func statsByIP(snap map[string]*ipStats) map[string]*ipAggView {
	out := make(map[string]*ipAggView, len(snap))
	for ip, is := range snap {
		var total int64
		for _, ke := range is.Keys {
			total += ke.Count
		}
		out[ip] = &ipAggView{
			Keys:         is.Keys,
			DistinctKeys: len(is.Keys),
			TotalCount:   total,
		}
	}
	return out
}

// keyAggView 是反向视图:某个 key 被哪些 IP 使用。
type keyAggView struct {
	IPs        map[string]*keyEntry `json:"ips"`
	DistinctIPs int                 `json:"distinct_ips"` // 该 key 触发了多少个不同 IP
	TotalCount  int64               `json:"total_count"`
}

func statsByKey(snap map[string]*ipStats) map[string]*keyAggView {
	out := make(map[string]*keyAggView)
	for ip, is := range snap {
		for k, ke := range is.Keys {
			kv, ok := out[k]
			if !ok {
				kv = &keyAggView{IPs: make(map[string]*keyEntry)}
				out[k] = kv
			}
			ke2 := *ke
			kv.IPs[ip] = &ke2
			kv.TotalCount += ke.Count
		}
	}
	for _, kv := range out {
		kv.DistinctIPs = len(kv.IPs)
	}
	return out
}

// renderStatsTable 渲染 ASCII 表格。
func renderStatsTable(w io.Writer, snap map[string]*ipStats, by string) {
	var rows []rowView
	if by == "key" {
		for ip, is := range snap {
			for k, ke := range is.Keys {
				rows = append(rows, rowView{
					Primary: k, Peer: ip,
					Count: ke.Count, LastSeen: ke.LastSeen,
					LastStatus: ke.LastStatus, LastTarget: ke.LastTarget,
				})
			}
		}
	} else {
		for ip, is := range snap {
			for k, ke := range is.Keys {
				rows = append(rows, rowView{
					Primary: ip, Peer: k,
					Count: ke.Count, LastSeen: ke.LastSeen,
					LastStatus: ke.LastStatus, LastTarget: ke.LastTarget,
				})
			}
		}
	}

	// 列名随维度变化。掩码 key 可能很长(40+字符),所以按 key 维度时
	// 主列放宽,对端列(IP)收窄。
	var primaryCol, peerCol string
	var primaryW, peerW int
	if by == "key" {
		primaryCol, peerCol = "KEY", "IP"
		primaryW, peerW = 48, 18
	} else {
		primaryCol, peerCol = "IP", "KEY"
		primaryW, peerW = 18, 48
	}
	totalW := primaryW + peerW + 6 + 6 + 26 + 12

	fmt.Fprintf(w, "%-*s %-*s %6s %6s %-26s %s\n",
		primaryW, primaryCol, peerW, peerCol, "COUNT", "STATUS", "LAST_SEEN", "TARGET")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", totalW))
	for _, r := range rows {
		ts := r.LastSeen.Format("2006-01-02 15:04:05")
		fmt.Fprintf(w, "%-*s %-*s %6d %6d %-26s %s\n",
			primaryW, trunc(r.Primary, primaryW), peerW, trunc(r.Peer, peerW),
			r.Count, r.LastStatus, ts, r.LastTarget)
	}
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", totalW))

	// 去重汇总
	if by == "key" {
		agg := statsByKey(snap)
		fmt.Fprintf(w, "\n去重统计(按 KEY):%d 个不同 key,共 %d 个不同 IP,总计调用 %d 次\n",
			len(agg), distinctIPCount(snap), totalCount(snap))
	} else {
		fmt.Fprintf(w, "\n去重统计(按 IP):%d 个不同 IP,共 %d 个不同 key,总计调用 %d 次\n",
			len(snap), distinctKeyCount(snap), totalCount(snap))
	}
}

// trunc 把字符串截断到 maxLen,超出加省略号(用于表格对齐)。
func trunc(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

func distinctIPCount(snap map[string]*ipStats) int {
	return len(snap)
}

func distinctKeyCount(snap map[string]*ipStats) int {
	seen := map[string]bool{}
	for _, is := range snap {
		for k := range is.Keys {
			seen[k] = true
		}
	}
	return len(seen)
}

func totalCount(snap map[string]*ipStats) int64 {
	var t int64
	for _, is := range snap {
		for _, ke := range is.Keys {
			t += ke.Count
		}
	}
	return t
}

// --- 采集函数 ------------------------------------------------------------

// clientIP 从请求里提取客户端真实 IP。
// 顺序:X-Forwarded-For(第一个)→ X-Real-IP → RemoteAddr(去掉端口)。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个,trim 空格
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// extractKey 按常见大模型 API 约定从请求头里提取鉴权 key(原始明文)。
// 找不到返回 ("", false)。
func extractKey(r *http.Request) (string, bool) {
	// 1. Authorization: Bearer <key>
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			k := strings.TrimSpace(auth[7:]) // len("bearer ") = 7
			if k != "" {
				return k, true
			}
		}
	}
	// 2. x-api-key (Anthropic Claude)
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k), true
	}
	// 3. api-key (Azure OpenAI 等)
	if k := r.Header.Get("api-key"); k != "" {
		return strings.TrimSpace(k), true
	}
	return "", false
}

// maskKey 把 key 掩码:保留前缀(到第一个 '-')+ 后 4 位,中间用 * 填充(至少 4 个)。
// 例:sk-abcd1234efgh5678 -> sk-ab**********5678
//     sk-ant-xxx...yyyy   -> sk-ant-***yyyy
//     mytoken             -> myto***oken  (无 '-' 时保留前 4 + 后 4)
func maskKey(k string) string {
	n := len(k)
	// 太短的全掩码,避免泄露
	if n <= 8 {
		return strings.Repeat("*", n)
	}

	prefix := ""
	if i := strings.Index(k, "-"); i >= 0 {
		prefix = k[:i+1] // 含 '-'
	} else {
		prefix = k[:4]
	}

	tail := k[n-4:]
	stars := strings.Repeat("*", 4)
	if n-len(prefix)-4 > 4 {
		stars = strings.Repeat("*", n-len(prefix)-4)
	}
	return prefix + stars + tail
}

// maskedKeyFromRequest 是 extractKey + maskKey 的组合,找不到返回 "-"。
func maskedKeyFromRequest(r *http.Request) string {
	k, ok := extractKey(r)
	if !ok {
		return "-"
	}
	return maskKey(k)
}

// logRequest 打一行结构化日志,只含 IP/掩码key/host/状态码,不含 body。
func logRequest(ip, maskedKey, method, targetHost string, status int, dur time.Duration) {
	log.Printf("req ip=%s key=%s method=%s host=%s status=%d dur=%s",
		ip, maskedKey, method, targetHost, status, dur)
}
