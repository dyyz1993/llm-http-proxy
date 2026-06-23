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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bjTime 是北京时间(UTC+8)的 time.Time 包装。
// JSON 序列化时自动转北京时区,不依赖服务器本地时区。
type bjTime time.Time

// beijing 是北京时区(UTC+8),全局共用(main.go 也引用)。
var beijing = time.FixedZone("CST", 8*3600)

// MarshalJSON 把时间按北京时间 RFC3339 输出。
func (t bjTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).In(beijing).Format(time.RFC3339) + `"`), nil
}

// UnmarshalJSON 从 JSON 字符串解析回 bjTime(支持 RFC3339)。
func (t *bjTime) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = bjTime(parsed)
	return nil
}

// bjFormat 按北京时间格式化,给表格输出用。
func (t bjTime) bjFormat(layout string) string {
	return time.Time(t).In(beijing).Format(layout)
}

// IsZero 透传到底层 time.Time。
func (t bjTime) IsZero() bool {
	return time.Time(t).IsZero()
}

// After 透传到底层 time.Time。
func (t bjTime) After(u bjTime) bool {
	return time.Time(t).After(time.Time(u))
}

// Equal 透传到底层 time.Time 的 Equal。
func (t bjTime) Equal(u bjTime) bool {
	return time.Time(t).Equal(time.Time(u))
}

// keyEntry 是单个 (IP, 掩码key) 的累计统计。
type keyEntry struct {
	Count        int64         `json:"count"`
	FirstSeen    bjTime        `json:"first_seen"` // 首次访问时间(创建时设,不变)
	LastSeen     bjTime        `json:"last_seen"`  // 最后访问时间
	LastStatus   int           `json:"last_status"`
	LastTarget   string        `json:"last_target"`   // 只记 host,不记 path
	StatusCounts map[int]int64 `json:"status_counts"` // 各状态码累计计数
}

// ipStats 是某个 IP 下的若干 key 的统计。
type ipStats struct {
	Keys map[string]*keyEntry // key = 掩码后的 key
}

// statsCollector 是全局统计收集器,线程安全。
type statsCollector struct {
	mu    sync.Mutex
	data  map[string]*ipStats // key = IP
	hours [24]hourBucket      // 最近 24 小时调用量(环形缓冲)
}

// hourBucket 是一个小时桶:起始时间 + 调用次数。
type hourBucket struct {
	Hour  time.Time // 该桶代表的小时(截断到整点)
	Count int64
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
		now := bjTime(time.Now())
		ke = &keyEntry{FirstSeen: now, StatusCounts: make(map[int]int64)} // 首次访问
		is.Keys[maskedKey] = ke
	}
	ke.Count++
	ke.LastSeen = bjTime(time.Now())
	ke.LastStatus = status
	ke.LastTarget = targetHost
	ke.StatusCounts[status]++

	// 时间桶:当前小时 +1
	s.bumpHour(time.Now())
}

// bumpHour 在当前小时桶上 +1,必要时滚动新桶(环形)。
func (s *statsCollector) bumpHour(now time.Time) {
	curHour := now.Truncate(time.Hour)
	slot := int(curHour.Unix() / 3600 % 24)
	b := &s.hours[slot]
	if !b.Hour.Equal(curHour) {
		// 新的小时,重置桶
		b.Hour = curHour
		b.Count = 0
	}
	b.Count++
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
			// 深拷贝 StatusCounts
			if ke.StatusCounts != nil {
				ke2.StatusCounts = make(map[int]int64, len(ke.StatusCounts))
				for k, v := range ke.StatusCounts {
					ke2.StatusCounts[k] = v
				}
			}
			is2.Keys[k] = &ke2
		}
		out[ip] = is2
	}
	return out
}

// hoursSnapshot 返回最近 N 小时的调用量(从最早到最近),跳过空桶。
func (s *statsCollector) hoursSnapshot(n int) []hourEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	curHour := now.Truncate(time.Hour)

	var out []hourEntry
	for i := n - 1; i >= 0; i-- {
		h := curHour.Add(-time.Duration(i) * time.Hour)
		slot := int(h.Unix() / 3600 % 24)
		b := s.hours[slot]
		// 只算 Hour 匹配的桶(不匹配说明是更早的数据,应视为空)
		count := int64(0)
		if b.Hour.Equal(h) {
			count = b.Count
		}
		out = append(out, hourEntry{Hour: h.In(beijing).Format("15:04"), Count: count})
	}
	return out
}

// hourEntry 是时间窗口输出的单条记录。
type hourEntry struct {
	Hour  string `json:"hour"`
	Count int64  `json:"count"`
}

// statsHandler 处理 GET /__stats,返回统计汇总。
//
// 查询参数:
//
//	by=ip       (默认)按来源 IP 聚合
//	by=key      按 key 聚合(反向)
//	by=window   返回最近 N 小时的调用量时间序列
//	format=json (默认)/ format=table ASCII 表格
//	top=N       只返回调用最多的 N 个(配合 by=ip/key)
//
// authCheck 非 nil 时,先校验鉴权(用于 /__stats 加密码保护)。
func statsHandler(s *statsCollector, authCheck func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 鉴权检查(如果配置了)
		if authCheck != nil && !authCheck(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		by := r.URL.Query().Get("by")
		if by != "key" && by != "window" {
			by = "ip" // 默认
		}
		format := r.URL.Query().Get("format")
		if format != "table" {
			format = "json" // 默认
		}
		top := 0
		if t := r.URL.Query().Get("top"); t != "" {
			if v, err := strconv.Atoi(t); err == nil && v > 0 {
				top = v
			}
		}

		// 时间窗口视图,单独处理
		if by == "window" {
			n := 24
			if w2 := r.URL.Query().Get("hours"); w2 != "" {
				if v, err := strconv.Atoi(w2); err == nil && v > 0 && v <= 24 {
					n = v
				}
			}
			if format == "table" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				renderWindowTable(w, s.hoursSnapshot(n))
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(struct {
				Window []hourEntry `json:"window"`
			}{s.hoursSnapshot(n)})
			return
		}

		snap := s.snapshot()
		if top > 0 {
			snap = topN(snap, by, top)
		}

		if format == "table" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			renderStatsTable(w, snap, by)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if by == "key" {
			json.NewEncoder(w).Encode(statsByKey(snap))
		} else {
			json.NewEncoder(w).Encode(statsByIP(snap))
		}
	}
}

// topN 返回调用量最高的前 N 个 IP(及其所有 key)。
func topN(snap map[string]*ipStats, by string, n int) map[string]*ipStats {
	type ipCount struct {
		ip    string
		total int64
	}
	var list []ipCount
	for ip, is := range snap {
		var t int64
		for _, ke := range is.Keys {
			t += ke.Count
		}
		list = append(list, ipCount{ip, t})
	}
	// 降序
	sort.Slice(list, func(i, j int) bool { return list[i].total > list[j].total })
	if n > len(list) {
		n = len(list)
	}
	out := make(map[string]*ipStats, n)
	for i := 0; i < n; i++ {
		out[list[i].ip] = snap[list[i].ip]
	}
	return out
}

// rowView 是表格/列表里的一行:一个 (维度值, 对端, 统计) 三元组。
type rowView struct {
	Primary    string // 维度值(IP 或 key)
	Peer       string // 对端(key 或 IP)
	Count      int64
	FirstSeen  bjTime
	LastSeen   bjTime
	LastStatus int
	LastTarget string
}

// statsByIP 生成按 IP 聚合的 JSON 视图,带去重计数。
type ipAggView struct {
	Keys         map[string]*keyEntry `json:"keys"`
	DistinctKeys int                  `json:"distinct_keys"`
	TotalCount   int64                `json:"total_count"`
	SuccessRate  float64              `json:"success_rate"` // 2xx 占比,0-1
}

func statsByIP(snap map[string]*ipStats) map[string]*ipAggView {
	out := make(map[string]*ipAggView, len(snap))
	for ip, is := range snap {
		var total, ok2xx int64
		for _, ke := range is.Keys {
			total += ke.Count
			ok2xx += count2xx(ke)
		}
		out[ip] = &ipAggView{
			Keys:         is.Keys,
			DistinctKeys: len(is.Keys),
			TotalCount:   total,
			SuccessRate:  ratio(ok2xx, total),
		}
	}
	return out
}

// keyAggView 是反向视图:某个 key 被哪些 IP 使用。
type keyAggView struct {
	IPs         map[string]*keyEntry `json:"ips"`
	DistinctIPs int                  `json:"distinct_ips"`
	TotalCount  int64                `json:"total_count"`
	SuccessRate float64              `json:"success_rate"`
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
		var ok2xx int64
		for _, ke := range kv.IPs {
			ok2xx += count2xx(ke)
		}
		kv.SuccessRate = ratio(ok2xx, kv.TotalCount)
	}
	return out
}

// count2xx 统计一个 keyEntry 里 2xx 状态码的次数。
func count2xx(ke *keyEntry) int64 {
	var n int64
	for code, c := range ke.StatusCounts {
		if code >= 200 && code < 300 {
			n += c
		}
	}
	return n
}

// ratio 计算 a/b,避免除零。
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// renderStatsTable 渲染 ASCII 表格。
func renderStatsTable(w io.Writer, snap map[string]*ipStats, by string) {
	var rows []rowView
	if by == "key" {
		for ip, is := range snap {
			for k, ke := range is.Keys {
				rows = append(rows, rowView{
					Primary: k, Peer: ip,
					Count: ke.Count, FirstSeen: ke.FirstSeen, LastSeen: ke.LastSeen,
					LastStatus: ke.LastStatus, LastTarget: ke.LastTarget,
				})
			}
		}
	} else {
		for ip, is := range snap {
			for k, ke := range is.Keys {
				rows = append(rows, rowView{
					Primary: ip, Peer: k,
					Count: ke.Count, FirstSeen: ke.FirstSeen, LastSeen: ke.LastSeen,
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
	totalW := primaryW + peerW + 6 + 6 + 19 + 19 + 12

	fmt.Fprintf(w, "%-*s %-*s %6s %6s %-19s %-19s %s\n",
		primaryW, primaryCol, peerW, peerCol, "COUNT", "STATUS", "FIRST_SEEN", "LAST_SEEN", "TARGET")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", totalW))
	for _, r := range rows {
		fs := r.FirstSeen.bjFormat("2006-01-02 15:04:05")
		ls := r.LastSeen.bjFormat("2006-01-02 15:04:05")
		fmt.Fprintf(w, "%-*s %-*s %6d %6d %-19s %-19s %s\n",
			primaryW, trunc(r.Primary, primaryW), peerW, trunc(r.Peer, peerW),
			r.Count, r.LastStatus, fs, ls, r.LastTarget)
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

// renderWindowTable 渲染时间窗口表格(最近 N 小时调用量)。
func renderWindowTable(w io.Writer, hours []hourEntry) {
	fmt.Fprintf(w, "%-12s %8s %s\n", "HOUR", "COUNT", "BAR")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 50))
	// 找最大值用于归一化柱状图
	var maxC int64
	for _, h := range hours {
		if h.Count > maxC {
			maxC = h.Count
		}
	}
	var total int64
	for _, h := range hours {
		var bar string
		if maxC > 0 {
			n := int(h.Count * 30 / maxC)
			bar = strings.Repeat("█", n)
		}
		fmt.Fprintf(w, "%-12s %8d %s\n", h.Hour, h.Count, bar)
		total += h.Count
	}
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(w, "总计 %d 小时,共 %d 次调用\n", len(hours), total)
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
//
//	sk-ant-xxx...yyyy   -> sk-ant-***yyyy
//	mytoken             -> myto***oken  (无 '-' 时保留前 4 + 后 4)
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

	// 如果 prefix 太长(prefix + tail 会重叠),全掩码保护避免明文泄露。
	// 例:"123456-789"(n=9, prefix="123456-")→ prefix+len("tail")=11 > 9 → 全掩码。
	if len(prefix)+4 >= n {
		return strings.Repeat("*", n)
	}

	tail := k[n-4:]
	gap := n - len(prefix) - 4
	if gap < 4 {
		gap = 4 // 至少 4 个 *
	}
	stars := strings.Repeat("*", gap)
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

// logEntry 是单条请求日志(给 admin UI 的日志页用)。
type logEntry struct {
	Time     string `json:"time"` // 北京时间
	IP       string `json:"ip"`
	Key      string `json:"key"` // 掩码 key 或 alias 标签
	Method   string `json:"method"`
	Host     string `json:"host"`
	Status   int    `json:"status"`
	Duration string `json:"dur"`
	TTFB     string `json:"ttfb"`   // 首字节响应时间(到第一个字节返回)
	Stream   bool   `json:"stream"` // 是否为 SSE 流式响应
	// token 用量(从响应 usage 提取,异步记录;0 表示没提取到)
	Prompt     int64 `json:"prompt"`     // 输入 token
	Cached     int64 `json:"cached"`     // 缓存命中 token
	Completion int64 `json:"completion"` // 输出 token
	// 费用(由 glm-cost 计算)
	CostCalculated bool    `json:"cost_calculated"` // 是否成功计算了费用
	InputCost      float64 `json:"input_cost"`      // 输入费用（元）
	OutputCost     float64 `json:"output_cost"`     // 输出费用（元）
	TotalCost      float64 `json:"total_cost"`      // 总费用（元）
}

// logRing 是内存环形缓冲,存最近 N 条请求日志(给 admin UI 看)。
type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	size    int
	next    int // 下一个写入位置
}

var globalLogRing = &logRing{size: 500, entries: make([]logEntry, 500)}

// add 追加一条日志到环形缓冲。
func (r *logRing) add(e logEntry) {
	r.mu.Lock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % r.size
	r.mu.Unlock()
}

// recent 返回最近 n 条日志(倒序,最新的在前)。
func (r *logRing) recent(n int) []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.size {
		n = r.size
	}
	out := make([]logEntry, 0, n)
	// 从最新(next-1)往回取
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + r.size) % r.size
		e := r.entries[idx]
		if e.Time == "" {
			break // 空槽位,说明还没写满
		}
		out = append(out, e)
	}
	return out
}

// logRequest 打一行结构化日志,只含 IP/掩码key/host/状态码/token用量/费用,不含 body。
// 同时写入内存环形缓冲(给 admin UI 看)。
// u 是从响应里异步提取的 token 用量(可能 HasData=false)。
// isStream 标记是否为 SSE 流式响应。
// ttfb 是首字节响应时间(从请求开始到第一个字节返回)。
func logRequest(ip, maskedKey, method, targetHost string, status int, dur, ttfb time.Duration, isStream bool, u usageData) {
	line := fmt.Sprintf("req ip=%s key=%s method=%s host=%s status=%d dur=%s",
		ip, maskedKey, method, targetHost, status, dur)
	if ttfb > 0 {
		line += fmt.Sprintf(" ttfb=%s", ttfb.Round(time.Millisecond))
	}
	if isStream {
		line += " stream=1"
	}
	if u.HasData {
		line += fmt.Sprintf(" prompt=%d cached=%d completion=%d", u.Prompt, u.Cached, u.Completion)
		if u.TotalCost > 0 {
			line += fmt.Sprintf(" cost=%.6f", u.TotalCost)
		}
	}
	log.Print(line)
	globalLogRing.add(logEntry{
		Time:           time.Now().In(beijing).Format("2006-01-02 15:04:05"),
		IP:             ip,
		Key:            maskedKey,
		Method:         method,
		Host:           targetHost,
		Status:         status,
		Duration:       dur.Round(time.Millisecond).String(),
		TTFB:           ttfb.Round(time.Millisecond).String(),
		Stream:         isStream,
		Prompt:         u.Prompt,
		Cached:         u.Cached,
		Completion:     u.Completion,
		CostCalculated: u.CostCalculated,
		InputCost:      u.InputCost,
		OutputCost:     u.OutputCost,
		TotalCost:      u.TotalCost,
	})
}

// --- 持久化 ---------------------------------------------------------------
//
// 把统计快照存到 JSON 文件,重启时读回。
// 写入采用原子方式:先写到临时文件,再 rename,避免崩溃时留下半个文件。

// persistSnapshot 是落盘的文件结构。版本号便于将来升级格式。
type persistSnapshot struct {
	Version int                 `json:"version"`
	Data    map[string]*ipStats `json:"data"`
}

// load 从 path 读取统计快照,恢复到 collector。文件不存在视为空,不报错。
func (s *statsCollector) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动,没文件很正常
		}
		return err
	}
	var snap persistSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = snap.Data
	if s.data == nil {
		s.data = make(map[string]*ipStats)
	}
	return nil
}

// save 把当前统计原子写入 path。
func (s *statsCollector) save(path string) error {
	snap := s.snapshot() // 已深拷贝,不持锁
	out := persistSnapshot{Version: 1, Data: snap}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	// 原子写:先写临时文件,再 rename
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// startPersistLoop 启动后台 goroutine,每 interval 落盘一次。
func (s *statsCollector) startPersistLoop(path string, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if err := s.save(path); err != nil {
				log.Printf("统计落盘失败: %v", err)
			}
		}
	}()
}
