// usage.go — 从 API 响应中提取 token 用量(prompt/cached/completion),
// 按 alias 聚合统计 + 计算缓存命中率。
//
// 两种格式:
//   - OpenAI:  {"usage":{"prompt_tokens":N,"prompt_tokens_details":{"cached_tokens":M},"completion_tokens":K}}
//   - Anthropic: {"usage":{"input_tokens":N,"cache_read_input_tokens":M,"output_tokens":K}}
//
// 支持普通 JSON 响应 + SSE 流式(扫最后一个含 usage 的 chunk)。
// 解析在 goroutine 里异步进行,不阻塞响应转发。

package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// usageData 是从单次响应里提取出的 token 用量。
type usageData struct {
	HasData    bool  // 是否成功提取到 usage
	Prompt     int64 // 输入 token(OpenAI:prompt_tokens / Anthropic:input_tokens)
	Cached     int64 // 缓存命中 token(OpenAI:cached_tokens / Anthropic:cache_read_input_tokens)
	Completion int64 // 输出 token(OpenAI:completion_tokens / Anthropic:output_tokens)
}

// openAIUsage 对应 OpenAI 格式的 usage 字段。
type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// anthropicUsage 对应 Anthropic 格式的 usage 字段。
type anthropicUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// extractUsage 从一段响应 body 里提取 token 用量。
// body 可能是:
//   - 完整 JSON(普通响应)
//   - SSE 流(多个 data: {...}\n\n,SSE 的最后一个 chunk 通常带 usage)
//   - 非法/不含 usage → 返回 HasData=false
//
// 自动识别 OpenAI / Anthropic 格式。
func extractUsage(body []byte) usageData {
	// 截断保护:body 太大(>2MB)只看末尾 512KB(usage 在最后)
	if len(body) > 2*1024*1024 {
		body = body[len(body)-512*1024:]
	}

	// 尝试方式 1:整体 JSON(非流式响应,支持嵌套格式)
	if u := tryJSONFlexible(body); u.HasData {
		return u
	}

	// 尝试方式 2:SSE 流式,扫每个 data: 行,取最后一个含 usage 的
	if u := trySSE(body); u.HasData {
		return u
	}

	return usageData{}
}

// tryJSON 尝试把 body 当成单个 JSON 解析。
func tryJSON(body []byte) usageData {
	// 快速判断:不含 "usage" 直接放弃(避免无谓的 json 解析)
	if !bytesContains(body, `"usage"`) {
		return usageData{}
	}

	// 尝试 OpenAI 格式
	var oi struct {
		Usage openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &oi) == nil && oi.Usage.PromptTokens > 0 {
		return usageData{
			HasData:    true,
			Prompt:     oi.Usage.PromptTokens,
			Cached:     oi.Usage.PromptTokensDetails.CachedTokens,
			Completion: oi.Usage.CompletionTokens,
		}
	}

	// 尝试 Anthropic 格式
	var ai struct {
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &ai) == nil && ai.Usage.InputTokens > 0 {
		return usageData{
			HasData:    true,
			Prompt:     ai.Usage.InputTokens,
			Cached:     ai.Usage.CacheReadInputTokens,
			Completion: ai.Usage.OutputTokens,
		}
	}

	return usageData{}
}

// trySSE 从 SSE 流里提取 usage。
// SSE 格式:多行 "data: {...}\n\n"。
// 策略:遍历所有 data: 行,对每个含 "usage" 的 chunk 尝试解析,
// 把找到的字段累积合并(因为 Anthropic 的 input/cache 在 message_start,
// output 在 message_delta;OpenAI 则全在最后一个 chunk)。
func trySSE(body []byte) usageData {
	s := string(body)
	var result usageData

	// 遍历每个 "data: " 行
	for {
		idx := strings.Index(s, "data: ")
		if idx < 0 {
			break
		}
		s = s[idx+6:]
		// 取到行尾
		line := s
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			line = s[:nl]
			s = s[nl+1:]
		}
		line = strings.TrimSpace(line)
		if line == "[DONE]" || line == "" {
			continue
		}
		// 尝试解析(支持嵌套的 Anthropic message_start 格式)
		if u := tryJSONFlexible([]byte(line)); u.HasData {
			result.HasData = true
			// 后找到的非零值覆盖前面的(合并)
			if u.Prompt > 0 {
				result.Prompt = u.Prompt
			}
			if u.Cached > 0 {
				result.Cached = u.Cached
			}
			if u.Completion > 0 {
				result.Completion = u.Completion
			}
		}
	}
	return result
}

// tryJSONFlexible 尝试多种 JSON 结构提取 usage:
//   - 顶层 usage(OpenAI 标准格式)
//   - message.usage(Anthropic message_start 事件)
func tryJSONFlexible(body []byte) usageData {
	if !bytesContains(body, `"usage"`) && !bytesContains(body, `"input_tokens"`) {
		return usageData{}
	}

	// 尝试 1:顶层 usage(OpenAI / Anthropic 非流式)
	if u := tryJSON(body); u.HasData {
		return u
	}

	// 尝试 2:Anthropic message_start 格式 {"message":{"usage":{...}}}
	var anthropicStart struct {
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &anthropicStart) == nil && anthropicStart.Message.Usage.InputTokens > 0 {
		return usageData{
			HasData:    true,
			Prompt:     anthropicStart.Message.Usage.InputTokens,
			Cached:     anthropicStart.Message.Usage.CacheReadInputTokens,
			Completion: anthropicStart.Message.Usage.OutputTokens,
		}
	}

	// 尝试 3:Anthropic message_delta {"usage":{"output_tokens":N}}
	// (input_tokens 可能为 0,只看 output)
	var anthropicDelta struct {
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &anthropicDelta) == nil && anthropicDelta.Usage.OutputTokens > 0 {
		return usageData{
			HasData:    true,
			Completion: anthropicDelta.Usage.OutputTokens,
		}
	}

	return usageData{}
}

// bytesContains 检查 body 是否含子串(避免引入 bytes 包的额外依赖)。
func bytesContains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}

// ---------- 按 alias 聚合统计 ----------

// aliasUsageStats 是单个 alias 的累计 token 统计。
type aliasUsageStats struct {
	Prompt     int64 // 累计输入 token
	Cached     int64 // 累计缓存命中 token
	Completion int64 // 累计输出 token
	Count      int64 // 成功提取到 usage 的请求次数
}

// cacheHitRate 计算缓存命中率 = cached / prompt。
// 即"输入 token 里有多少比例命中了缓存"。
func (s aliasUsageStats) cacheHitRate() float64 {
	if s.Prompt == 0 {
		return 0
	}
	return float64(s.Cached) / float64(s.Prompt)
}

// usageStats 持有所有 alias 的 token 用量统计。
type usageStats struct {
	mu   sync.RWMutex
	data map[string]*aliasUsageStats // key = alias
}

func newUsageStats() *usageStats {
	return &usageStats{data: make(map[string]*aliasUsageStats)}
}

// record 异步记录一次请求的 token 用量到指定 alias。
// 安全地在 goroutine 里调用。
func (us *usageStats) record(alias string, u usageData) {
	if !u.HasData || alias == "" {
		return
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	s := us.data[alias]
	if s == nil {
		s = &aliasUsageStats{}
		us.data[alias] = s
	}
	atomic.AddInt64(&s.Prompt, u.Prompt)
	atomic.AddInt64(&s.Cached, u.Cached)
	atomic.AddInt64(&s.Completion, u.Completion)
	atomic.AddInt64(&s.Count, 1)
}

// snapshot 返回所有 alias 统计的快照(给 Dashboard 展示用)。
func (us *usageStats) snapshot() map[string]aliasUsageStats {
	us.mu.RLock()
	defer us.mu.RUnlock()
	out := make(map[string]aliasUsageStats, len(us.data))
	for k, v := range us.data {
		out[k] = *v
	}
	return out
}

// ---------- Dashboard 展示 ----------

// buildUsageHTML 构建按 alias 聚合的 token 用量展示 HTML。
// 展示:每个 alias 的累计输入/缓存/输出 token + 平均缓存命中率。
func buildUsageHTML(snap map[string]aliasUsageStats) string {
	if len(snap) == 0 {
		return ""
	}

	// 按 alias 排序
	aliases := make([]string, 0, len(snap))
	for a := range snap {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	var b strings.Builder
	b.WriteString(`<table style="font-size:13px;margin-top:8px">`)
	b.WriteString(`<tr><th>Alias</th><th>请求数</th><th>输入</th><th>缓存命中</th><th>输出</th><th>命中率</th></tr>`)

	var totalPrompt, totalCached, totalCompletion int64
	var totalCount int64
	for _, alias := range aliases {
		s := snap[alias]
		rate := s.cacheHitRate()
		// 命中率截断到 0..1(cached 可能 >> prompt,因为上游 cached 是累积口径)
		displayRate := rate
		if displayRate > 1 {
			displayRate = 1
		}
		// 进度条
		barLen := 20
		filled := int(displayRate * float64(barLen))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
		fmt.Fprintf(&b, `<tr><td><b>%s</b></td><td>%d</td><td>%s</td><td>%s</td><td>%s</td>`,
			alias, s.Count, fmtTokens(s.Prompt), fmtTokens(s.Cached), fmtTokens(s.Completion))
		fmt.Fprintf(&b, `<td><span style="font-family:monospace;font-size:11px">%s</span> %.1f%%</td></tr>`,
			bar, displayRate*100)
		totalPrompt += s.Prompt
		totalCached += s.Cached
		totalCompletion += s.Completion
		totalCount += s.Count
	}

	// 合计行
	var totalRate float64
	if totalPrompt > 0 {
		totalRate = float64(totalCached) / float64(totalPrompt)
	}
	if totalRate > 1 {
		totalRate = 1
	}
	fmt.Fprintf(&b, `<tr style="font-weight:bold;background:#eee"><td>合计</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td>`,
		totalCount, fmtTokens(totalPrompt), fmtTokens(totalCached), fmtTokens(totalCompletion))
	fmt.Fprintf(&b, `<td>%.1f%%</td></tr>`, totalRate*100)
	b.WriteString(`</table>`)
	return b.String()
}

// fmtTokens 把 token 数量格式化成易读的形式。
// <1000 → 原样; >=1000 → 1.2K; >=1000000 → 1.2M。
func fmtTokens(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
