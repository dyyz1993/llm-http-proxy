// quota.go — 定时从 api.z.ai 拉取所有 API key 的配额数据,缓存到内存给 Admin UI 展示。
//
// 流程:
//   keyStore 每 5 分钟遍历 keys.yaml 里所有 key,调 api.z.ai 的 /api/monitor/usage/quota/limit
//   接口获取每个 key 的限额数据(周期额度/周额度/月度时长等),缓存在 quotaCache 里。
//   Dashboard 页面直接从缓存读,不阻塞页面加载。
//
// 注意:配额数据是 api.z.ai 侧的统计,仅供展示参考,不参与代理转发逻辑。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- API 响应结构(只取需要的字段) ----------

// quotaLimit 对应 api.z.ai 返回的 limits 数组元素。
type quotaLimit struct {
	Type        string             `json:"type"`                   // TOKENS_LIMIT / TIME_LIMIT
	Unit        int                `json:"unit"`                   // 3=周期内 5=月度 6=日
	Number      int                `json:"number"`                 // 次数
	Percentage  int                `json:"percentage"`             // 已用百分比 0-100
	NextResetMs int64              `json:"nextResetTime"`          // 下次重置时间(ms)
	Usage       *int               `json:"usage,omitempty"`        // TIME_LIMIT 总配额
	CurrentVal  *int               `json:"currentValue,omitempty"` // TIME_LIMIT 已用
	Remaining   *int               `json:"remaining,omitempty"`    // TIME_LIMIT 剩余
	Details     []quotaUsageDetail `json:"usageDetails,omitempty"`
}

type quotaUsageDetail struct {
	ModelCode string `json:"modelCode"`
	Usage     int    `json:"usage"`
}

type quotaData struct {
	Level  string       `json:"level"` // pro / max
	Limits []quotaLimit `json:"limits"`
}

type quotaResponse struct {
	Code    int        `json:"code"`
	Success bool       `json:"success"`
	Msg     string     `json:"msg"`
	Data    *quotaData `json:"data,omitempty"`
}

// ---------- 缓存 ----------

// cachedQuota 是单个 key 的缓存条目。
type cachedQuota struct {
	Alias     string // keys.yaml 里的别名
	Level     string // pro / max / 未知
	Limits    []quotaLimit
	FetchedAt time.Time // 拉取时间
}

// quotaCache 持有所 key 的配额缓存,后台定时刷新。
type quotaCache struct {
	mu        sync.RWMutex
	entries   []cachedQuota // 按 alias 排序
	interval  time.Duration
	localAddr string // 本地代理地址(如 ":8080"),probe 走自己的代理
}

func newQuotaCache(localAddr string) *quotaCache {
	return &quotaCache{interval: 5 * time.Minute, localAddr: localAddr}
}

// fetchAll 遍历 keys.yaml 里所有 key,逐个调 api.z.ai 拉配额。
// 跳过那些 key 明显不是有效 token 的(比如以 "sk-" 开头的模拟 key)。
func (qc *quotaCache) fetchAll(configs map[string]KeyConfig) {
	var results []cachedQuota

	for alias, cfg := range configs {
		rawKey := cfg.Key
		// 跳过明显是模拟/无效的 key(如 "sk-test"、"sk-" 等)
		if strings.HasPrefix(rawKey, "sk-") || len(rawKey) < 20 {
			continue
		}

		data := qc.fetchOne(alias, rawKey)
		if data != nil {
			results = append(results, *data)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Alias < results[j].Alias
	})

	qc.mu.Lock()
	qc.entries = results
	qc.mu.Unlock()

	log.Printf("配额缓存刷新完成: %d 个 key", len(results))
}

// probeAndRefresh 在额度重置点触发后端结算,然后刷新配额。
// 两种 probe 都走自己的代理(/k/{alias}/),密钥服务端注入,代码里不碰 key。
// 按 key 去重:多个 alias 用同一个 key 时,只 probe 第一个,避免重复请求。
// 只 probe 没有周额度的 key(有周额度兜底的不需要提前抢占)。
func (qc *quotaCache) probeAndRefresh(ks *keyStore) {
	configs := ks.allConfigs()
	entries := qc.getAll()
	entriesByAlias := make(map[string]cachedQuota)
	for _, e := range entries {
		entriesByAlias[e.Alias] = e
	}

	// 按 key 去重:同一 key 只保留第一个遇到的 alias
	seen := make(map[string]bool) // rawKey → 已 probe
	for alias, cfg := range configs {
		rawKey := cfg.Key
		// 跳过无效 key
		if strings.HasPrefix(rawKey, "sk-") || len(rawKey) < 20 {
			continue
		}
		if seen[rawKey] {
			continue // 同一个 key 已经 probe 过,跳过
		}

		// 只 probe 没有周额度的 key(有周额度兜底的不需要)
		if entry, ok := entriesByAlias[alias]; ok {
			if hasWeeklyQuota(entry) {
				continue
			}
		}

		seen[rawKey] = true

		// 1. 调配额接口(0 token):走代理,触发后端结算
		qc.probeViaProxy(alias, "https://api.z.ai/api/monitor/usage/quota/limit")
		// 2. 调最小模型请求(~1 token):走代理,确保 key 可用 + 触发结算
		qc.probeModelViaProxy(alias, "glm-4-flash")
	}

	// probe 完再拉一次配额(此时后端已结算,数字准确)
	qc.fetchAll(configs)
}

// hasWeeklyQuota 检查 key 是否有周额度(unit=6)。
// 有周额度的 key 不需要提前 probe(周期额度到期了还有周额度兜底)。
func hasWeeklyQuota(entry cachedQuota) bool {
	for _, lim := range entry.Limits {
		if lim.Unit == 6 {
			return true
		}
	}
	return false
}

// probeViaProxy 通过自己的代理 /k/{alias}/{target} 发 GET 请求。
// 密钥由代理注入,这里不碰 key。用于调配额接口(0 消耗)。
func (qc *quotaCache) probeViaProxy(alias, targetURL string) {
	url := fmt.Sprintf("http://127.0.0.1%s/k/%s/%s", qc.localAddr, alias, targetURL)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("probe(配额)失败 [%s]: %v", alias, err)
		return
	}
	resp.Body.Close()
	log.Printf("probe(配额)已触发 [%s]: HTTP %d", alias, resp.StatusCode)
}

// probeModelViaProxy 通过代理发一个最小模型请求(走 /k/{alias}/)。
// 密钥服务端注入。消耗 ~1 token,确保 key 可用且触发结算。
// 走 coding 接口(coding 套餐 key 兼容)。
func (qc *quotaCache) probeModelViaProxy(alias, model string) {
	url := fmt.Sprintf("http://127.0.0.1%s/k/%s/https://api.z.ai/api/coding/paas/v4/chat/completions",
		qc.localAddr, alias)
	body := strings.NewReader(`{"model":"` + model + `","messages":[{"role":"user","content":"1"}],"max_tokens":1}`)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", body)
	if err != nil {
		log.Printf("probe(模型)失败 [%s]: %v", alias, err)
		return
	}
	resp.Body.Close()
	log.Printf("probe(模型)已触发 [%s] %s: HTTP %d (~1 token)", alias, model, resp.StatusCode)
}

// fetchOne 拉取单个别名的配额。
func (qc *quotaCache) fetchOne(alias, rawKey string) *cachedQuota {
	req, err := http.NewRequest(http.MethodGet,
		"https://api.z.ai/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", rawKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("配额拉取失败 [%s]: %v", alias, err)
		return nil
	}
	defer resp.Body.Close()

	var qr quotaResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		log.Printf("配额解析失败 [%s]: %v", alias, err)
		return nil
	}
	if !qr.Success || qr.Data == nil {
		log.Printf("配额接口返回失败 [%s]: %s", alias, qr.Msg)
		return nil
	}

	return &cachedQuota{
		Alias:     alias,
		Level:     qr.Data.Level,
		Limits:    qr.Data.Limits,
		FetchedAt: time.Now(),
	}
}

// getAll 返回所有缓存的配额快照,供 admin UI 展示。
func (qc *quotaCache) getAll() []cachedQuota {
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	out := make([]cachedQuota, len(qc.entries))
	copy(out, qc.entries)
	return out
}

// refreshNow 立即同步刷新配额缓存(不等 ticker),返回刷新到的 key 数量。
// 给 Dashboard 的手动"刷新"按钮用——刚加完 key 不用干等 5 分钟。
func (qc *quotaCache) refreshNow(ks *keyStore) int {
	if ks == nil {
		return 0
	}
	qc.fetchAll(ks.allConfigs())
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	return len(qc.entries)
}

// startLoop 后台刷新配额缓存。
// 四种触发:
//  1. 固定 5 分钟轮询(保底,确保数据不会太旧)
//  2. 提前抢占:重置点前 1 分钟发 probe,主动触发窗口滚动
//  3. 重置点定时器:重置点 +5s 再次 probe + 刷新(确认数字准确)
//  4. 窗口激活:对周期额度"无重置时间"的 key 定期发 probe,主动启动 5h 窗口
func (qc *quotaCache) startLoop(ks *keyStore) {
	go func() {
		// 启动后立即拉一次
		qc.fetchAll(ks.allConfigs())

		// 5 分钟保底轮询
		ticker := time.NewTicker(qc.interval)
		defer ticker.Stop()

		// 窗口激活定时器(每 4 小时对没窗口的 pro key 发 probe)
		activateTicker := time.NewTicker(4 * time.Hour)
		defer activateTicker.Stop()

		// 提前抢占定时器(重置点前 1 分钟)
		var preemptTimer *time.Timer
		// 重置点定时器(动态计算最近的重置时刻)
		var resetTimer *time.Timer

		scheduleResetTimers := func() {
			t := qc.nextResetTime()
			if t.IsZero() {
				return
			}
			d := time.Until(t)

			// 停掉旧的定时器
			if preemptTimer != nil {
				preemptTimer.Stop()
			}
			if resetTimer != nil {
				resetTimer.Stop()
			}

			// 提前抢占:重置点前 1 分钟发 probe,主动触发窗口滚动
			preemptD := d - time.Minute
			if preemptD > 30*time.Second {
				preemptTimer = time.AfterFunc(preemptD, func() {
					log.Printf("提前抢占: 重置点前 1 分钟,触发 probe")
					qc.probeAndRefresh(ks)
				})
			}

			// 重置点 +5s:确认数字准确
			if d > 0 {
				resetTimer = time.AfterFunc(d+5*time.Second, func() {
					log.Printf("重置点到达,触发 probe + 刷新")
					qc.probeAndRefresh(ks)
				})
			}
		}

		// 主循环
		for {
			select {
			case <-ticker.C:
				qc.fetchAll(ks.allConfigs())
				scheduleResetTimers()
			case <-activateTicker.C:
				// 窗口激活:对周期额度"无重置时间"的 pro key 发 probe
				qc.activateDormantWindows(ks)
			}
		}
	}()
}

// activateDormantWindows 对周期额度"无重置时间"的 key 发 probe 模型请求,
// 主动启动 5h 滚动窗口。只对没有周额度的 key 生效(有周额度兜底的不需要)。
func (qc *quotaCache) activateDormantWindows(ks *keyStore) {
	entries := qc.getAll()
	configs := ks.allConfigs()

	// 建索引:alias → 配额缓存
	entriesByAlias := make(map[string]cachedQuota)
	for _, e := range entries {
		entriesByAlias[e.Alias] = e
	}

	// 按 key 去重
	seen := make(map[string]bool)
	activated := 0
	for alias, cfg := range configs {
		rawKey := cfg.Key
		if strings.HasPrefix(rawKey, "sk-") || len(rawKey) < 20 {
			continue
		}
		if seen[rawKey] {
			continue
		}
		seen[rawKey] = true

		entry, ok := entriesByAlias[alias]
		if !ok {
			continue
		}

		// 有周额度的不需要激活
		if hasWeeklyQuota(entry) {
			continue
		}

		// 检查周期额度(unit=3)是否有重置时间
		// 无重置时间 = 窗口未启动,需要发 probe 激活
		needsActivation := false
		for _, lim := range entry.Limits {
			if lim.Unit == 3 && lim.NextResetMs <= 0 {
				needsActivation = true
				break
			}
		}

		if !needsActivation {
			continue
		}

		log.Printf("窗口激活: %s 周期额度无重置时间,发 probe 激活 5h 窗口", alias)
		// 发一个最小模型请求触发窗口启动
		qc.probeModelViaProxy(alias, "glm-4-flash")
		activated++
	}

	if activated > 0 {
		log.Printf("窗口激活完成: 激活了 %d 个 key", activated)
		// 激活后刷新一次配额
		qc.fetchAll(configs)
	}
}

// nextResetTime 返回缓存里所有 limit 中最近的一次重置时刻(已排除过期的)。
// 没有数据返回零值。
func (qc *quotaCache) nextResetTime() time.Time {
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	var earliest time.Time
	now := time.Now()
	for _, e := range qc.entries {
		for _, lim := range e.Limits {
			if lim.NextResetMs <= 0 {
				continue
			}
			t := time.Unix(lim.NextResetMs/1000, 0)
			// 只考虑未来的重置点
			if t.After(now) {
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
		}
	}
	return earliest
}

// ---------- 格式化工具(给模板用) ----------

// unitLabel 返回 unit 代码的中文描述。
// z.ai 的配额维度:
//
//	unit=3: 周期内(5小时滚动窗口)
//	unit=6: 周额度(max 套餐才有,滚动重置)
//	unit=5: 月度时长(月底重置)
func unitLabel(unit int) string {
	switch unit {
	case 3:
		return "周期额度"
	case 5:
		return "月度时长"
	case 6:
		return "周额度"
	default:
		return fmt.Sprintf("额度(%d)", unit)
	}
}

// progressBar 渲染 unicode 进度条字符串。
// 总宽 20 字符: █ 表示已用, ░ 表示剩余。
func progressBar(pct int) string {
	const barLen = 20
	filled := pct * barLen / 100
	if filled > barLen {
		filled = barLen
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
}

// fmtResetTime 把毫秒时间戳格式化成北京时间。
func fmtResetTime(ms int64) string {
	t := time.Unix(ms/1000, 0).In(beijing)
	now := time.Now().In(beijing)
	diff := t.Sub(now)

	if diff <= 0 {
		return "已到期"
	}
	if diff < 24*time.Hour {
		return t.Format("今天 15:04")
	}
	if diff < 48*time.Hour {
		return t.Format("明天 15:04")
	}
	return t.Format("1月2日 15:04")
}

// buildQuotaHTML 构建配额展示的 HTML 片段,供 dashboard 模板嵌入。
func buildQuotaHTML(entries []cachedQuota) string {
	if len(entries) == 0 {
		return `<p style="color:#999">暂无配额数据(需在 keys.yaml 中配置有效的 API key)</p>`
	}

	var b strings.Builder
	for _, e := range entries {
		levelIcon := "🅿️"
		if e.Level == "max" {
			levelIcon = "🅼"
		}

		fmt.Fprintf(&b, `<div style="margin:12px 0;padding:10px;background:#fff;border-radius:6px;border:1px solid #ddd">`)
		fmt.Fprintf(&b, `<div style="font-weight:bold;margin-bottom:6px">%s %s %s</div>`,
			e.Alias, levelIcon, e.Level)

		for _, lim := range e.Limits {
			bar := progressBar(lim.Percentage)
			resetStr := fmtResetTime(lim.NextResetMs)
			label := unitLabel(lim.Unit)

			fmt.Fprintf(&b,
				`<div style="font-size:13px;margin:4px 0;display:flex;align-items:center;gap:8px">`+
					`<span style="width:70px;flex-shrink:0">%s</span>`+
					`<span style="font-family:monospace;font-size:12px">%s</span>`+
					`<span style="width:40px;text-align:right">%d%%</span>`+
					`<span style="color:#888;font-size:12px">%s</span>`,
				label, bar, lim.Percentage, resetStr)

			// TIME_LIMIT 有配额明细时也展示
			if lim.Usage != nil && lim.CurrentVal != nil {
				fmt.Fprintf(&b, `<span style="color:#999;font-size:11px">(%d/%d秒)</span>`,
					*lim.CurrentVal, *lim.Usage)
			}
			b.WriteString(`</div>`)

			// 模型明细(Time 维度有按模型拆分)
			if len(lim.Details) > 0 {
				var parts []string
				for _, d := range lim.Details {
					if d.Usage > 0 {
						parts = append(parts, fmt.Sprintf("%s:%d", d.ModelCode, d.Usage))
					}
				}
				if len(parts) > 0 {
					fmt.Fprintf(&b, `<div style="font-size:11px;color:#999;margin-left:78px">模型: %s</div>`,
						strings.Join(parts, "  "))
				}
			}
		}

		fmt.Fprintf(&b, `<div style="font-size:11px;color:#bbb;margin-top:4px">更新于 %s</div>`,
			e.FetchedAt.In(beijing).Format("15:04:05"))
		b.WriteString(`</div>`)
	}
	return b.String()
}

// ---------- 直接查询原始 API Key ----------

// fetchOneKey 直接调 api.z.ai 拉取单个 raw key 的配额数据,不依赖 quotaCache。
func fetchOneKey(label, rawKey string) *cachedQuota {
	req, err := http.NewRequest(http.MethodGet,
		"https://api.z.ai/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", rawKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var qr quotaResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil
	}
	if !qr.Success || qr.Data == nil {
		return nil
	}

	return &cachedQuota{
		Alias:     label,
		Level:     qr.Data.Level,
		Limits:    qr.Data.Limits,
		FetchedAt: time.Now(),
	}
}

// ---------- 纯文本/ASCII 输出(按周额度排序) ----------

// asciiBar 渲染纯 ASCII 进度条,宽 20 格。
func asciiBar(pct int) string {
	const barLen = 20
	filled := pct * barLen / 100
	if filled > barLen {
		filled = barLen
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", barLen-filled) + "]"
}

// weeklyQuotaPct 提取周额度(unit=6)的使用百分比,没有周额度返回 -1。
func weeklyQuotaPct(e cachedQuota) int {
	for _, lim := range e.Limits {
		if lim.Unit == 6 {
			return lim.Percentage
		}
	}
	return -1
}

// weeklyQuotaRemaining 返回周额度剩余值(秒),没有周额度返回 -1。
func weeklyQuotaRemaining(e cachedQuota) int {
	for _, lim := range e.Limits {
		if lim.Unit == 6 && lim.Remaining != nil {
			return *lim.Remaining
		}
	}
	return -1
}

// buildSortedQuotaText 以纯文本格式输出所有 key 的配额数据,
// 按周额度使用率降序排列(用得最多的排最前),无周额度的排最后。
// filterAliases 非空时只输出指定 alias。
func buildSortedQuotaText(entries []cachedQuota, filterAliases ...string) string {
	if len(entries) == 0 {
		return "暂无配额数据\n"
	}

	// 按周额度百分比降序排序
	sorted := make([]cachedQuota, 0, len(entries))
	for _, e := range entries {
		if len(filterAliases) > 0 {
			matched := false
			for _, a := range filterAliases {
				if e.Alias == a {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		sorted = append(sorted, e)
	}
	if len(sorted) == 0 {
		return "没有匹配的 alias\n"
	}

	sort.SliceStable(sorted, func(i, j int) bool {
		pi := weeklyQuotaPct(sorted[i])
		pj := weeklyQuotaPct(sorted[j])
		if pi != pj {
			if pi == -1 {
				return false // 无周额度的排最后
			}
			if pj == -1 {
				return true
			}
			return pi > pj // 降序:用得多的排前面
		}
		return sorted[i].Alias < sorted[j].Alias
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%-4s %-16s %-6s %7s  %-24s %s\n",
		"排名", "Alias", "套餐", "周额度", "进度条", "重置时间")
	b.WriteString(strings.Repeat("-", 75) + "\n")

	for rank, e := range sorted {
		levelTag := "PRO"
		if e.Level == "max" {
			levelTag = "MAX"
		}
		pct := weeklyQuotaPct(e)
		bar := ""
		resetStr := ""
		if pct >= 0 {
			bar = asciiBar(pct)
			// 找周额度的重置时间
			for _, lim := range e.Limits {
				if lim.Unit == 6 {
					resetStr = fmtResetTime(lim.NextResetMs)
					break
				}
			}
		} else {
			bar = "(无周额度)"
		}
		pctStr := "-"
		if pct >= 0 {
			pctStr = fmt.Sprintf("%d%%", pct)
		}
		fmt.Fprintf(&b, "%-4d %-16s %-6s %7s  %-24s %s\n",
			rank+1, e.Alias, levelTag, pctStr, bar, resetStr)

		// 详细配额信息(缩进)
		for _, lim := range e.Limits {
			detailBar := asciiBar(lim.Percentage)
			detailReset := fmtResetTime(lim.NextResetMs)
			label := unitLabel(lim.Unit)
			fmt.Fprintf(&b, "       %-8s %s %3d%%  %s\n",
				label, detailBar, lim.Percentage, detailReset)

			// 时长类显示已用/剩余
			if lim.Usage != nil && lim.CurrentVal != nil {
				remaining := 0
				if lim.Remaining != nil {
					remaining = *lim.Remaining
				}
				fmt.Fprintf(&b, "              已用 %d/%d  剩余 %d\n",
					*lim.CurrentVal, *lim.Usage, remaining)
			}

			// 模型明细
			if len(lim.Details) > 0 {
				var parts []string
				for _, d := range lim.Details {
					if d.Usage > 0 {
						parts = append(parts, fmt.Sprintf("%s:%d", d.ModelCode, d.Usage))
					}
				}
				if len(parts) > 0 {
					fmt.Fprintf(&b, "       模型:   %s\n", strings.Join(parts, "  "))
				}
			}
		}
		_ = levelTag
	}
	b.WriteString(strings.Repeat("-", 75) + "\n")
	return b.String()
}
