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
	mu       sync.RWMutex
	entries  []cachedQuota // 按 alias 排序
	interval time.Duration
}

func newQuotaCache() *quotaCache {
	return &quotaCache{interval: 5 * time.Minute}
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

// startLoop 后台定时刷新配额缓存。
func (qc *quotaCache) startLoop(ks *keyStore) {
	go func() {
		// 启动后立即拉一次
		qc.fetchAll(ks.allConfigs())

		ticker := time.NewTicker(qc.interval)
		defer ticker.Stop()
		for range ticker.C {
			qc.fetchAll(ks.allConfigs())
		}
	}()
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
