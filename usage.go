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
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// usageData 是从单次响应里提取出的 token 用量与费用。
type usageData struct {
	HasData        bool    // 是否成功提取到 usage
	Model          string  // 模型名称(从响应提取,如 "glm-5.1-flash")
	Prompt         int64   // 输入 token(OpenAI:prompt_tokens / Anthropic:input_tokens)
	Cached         int64   // 缓存命中 token(OpenAI:cached_tokens / Anthropic:cache_read_input_tokens)
	Completion     int64   // 输出 token(OpenAI:completion_tokens / Anthropic:output_tokens)
	CostCalculated bool    // 是否成功计算了费用(模型在定价表中)
	InputCost      float64 // 输入费用（元,由调用方计算填入）
	OutputCost     float64 // 输出费用（元）
	TotalCost      float64 // 总费用（元）
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
	// 截断保护:非 SSE 的 body 太大(>2MB)只看末尾 512KB(usage 在最后)。
	// 注意:SSE 内容不在这里截断!SSE 的第一个 chunk 含 model(在开头),
	// 截断到末尾会丢掉 model → 费用算不出。SSE 的滑动窗口在 main.go 已处理。
	if len(body) > 2*1024*1024 && !bytesContains(body, "data: ") {
		body = body[len(body)-512*1024:]
	}

	// 尝试方式 1:整体 JSON(非流式响应,支持嵌套格式)
	if u := tryJSONFlexible(body); u.HasData {
		return u
	}

	// 尝试方式 2:SSE 流式,扫每个 data: 行,取最后一个含 usage 的
	u := trySSE(body)
	if !u.HasData && len(body) > 0 {
		// DEBUG: 提取失败时记录诊断信息(前 200 字节 + 后 200 字节)
		head, tail := body, body
		if len(head) > 200 {
			head = head[:200]
		}
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		log.Printf("extractUsage SSE 失败: bodyLen=%d head=%q tail=%q", len(body), string(head), string(tail))
	}
	return u
}

// tryJSON 尝试把 body 当成单个 JSON 解析。
func tryJSON(body []byte) usageData {
	// 快速判断:不含 "usage" 直接放弃(避免无谓的 json 解析)
	if !bytesContains(body, `"usage"`) {
		return usageData{}
	}

	// 尝试 OpenAI 格式
	var oi struct {
		Model string      `json:"model"`
		Usage openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &oi) == nil && oi.Usage.PromptTokens > 0 {
		return usageData{
			HasData:    true,
			Model:      oi.Model,
			Prompt:     oi.Usage.PromptTokens,
			Cached:     oi.Usage.PromptTokensDetails.CachedTokens,
			Completion: oi.Usage.CompletionTokens,
		}
	}

	// 尝试 Anthropic 格式
	// 注意语义差异:Anthropic 的 input_tokens 只是"新增输入"(不含缓存),
	// 总输入 = input_tokens + cache_read_input_tokens。
	// OpenAI 的 prompt_tokens 已经是总输入(含缓存)。
	// 为了统一命中率计算(cached/prompt),Anthropic 的 Prompt 要加上 cache_read。
	var ai struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &ai) == nil && (ai.Usage.InputTokens > 0 || ai.Usage.CacheReadInputTokens > 0) {
		return usageData{
			HasData:    true,
			Model:      ai.Model,
			Prompt:     ai.Usage.InputTokens + ai.Usage.CacheReadInputTokens, // 总输入
			Cached:     ai.Usage.CacheReadInputTokens,
			Completion: ai.Usage.OutputTokens,
		}
	}

	return usageData{}
}

// trySSE 从 SSE 流里提取 usage + model。
// SSE 格式:多行 "data: {...}\n\n"。
// 策略:遍历所有 data: 行,
//   - model 字段:从任意含 "model" 的 chunk 提取(通常在第一个 chunk)
//   - usage 字段:对每个含 "usage" 的 chunk 解析,累积合并
//     (Anthropic 的 input/cache 在 message_start,output 在 message_delta;
//     OpenAI 全在最后一个 chunk)
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

		// 1. 先单独提取 model 字段(不依赖 usage,因为 model 和 usage
		//    可能在不同 chunk —— GLM/OpenAI 的 SSE,model 在第一个 chunk,
		//    usage 在最后一个)
		if result.Model == "" && bytesContains([]byte(line), `"model"`) {
			var mc struct {
				Model string `json:"model"`
			}
			if json.Unmarshal([]byte(line), &mc) == nil && mc.Model != "" {
				result.Model = mc.Model
			}
		}

		// 2. 再尝试提取 usage(支持嵌套的 Anthropic message_start 格式)
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
			// usage chunk 里也可能带 model(兜底)
			if u.Model != "" && result.Model == "" {
				result.Model = u.Model
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
	// 注意:input_tokens 只是新增部分,总输入 = input + cache_read
	var anthropicStart struct {
		Type    string `json:"type"`
		Message struct {
			Model string         `json:"model"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &anthropicStart) == nil {
		au := anthropicStart.Message.Usage
		if au.InputTokens > 0 || au.CacheReadInputTokens > 0 {
			return usageData{
				HasData:    true,
				Model:      anthropicStart.Message.Model,
				Prompt:     au.InputTokens + au.CacheReadInputTokens,
				Cached:     au.CacheReadInputTokens,
				Completion: au.OutputTokens,
			}
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

// aliasUsageStats 是单个 alias 的累计 token 统计与费用统计。
type aliasUsageStats struct {
	Prompt     int64   // 累计输入 token
	Cached     int64   // 累计缓存命中 token
	Completion int64   // 累计输出 token
	Count      int64   // 成功提取到 usage 的请求次数
	InputCost  float64 // 累计输入费用（元）
	OutputCost float64 // 累计输出费用（元）
	TotalCost  float64 // 累计总费用（元）
	Errors     int64   // 错误请求数(4xx/5xx)
}

// cacheHitRate 计算缓存命中率 = cached / (prompt + cached)。
// z.ai 的 prompt_tokens 是"未缓存的输入部分",cached_tokens 是"已缓存的输入部分",
// 所以总输入 = prompt + cached,命中率 = cached / 总输入。
// 这样命中率永远在 0..1 之间,不会超过 100%。
func (s aliasUsageStats) cacheHitRate() float64 {
	total := s.Prompt + s.Cached
	if total == 0 {
		return 0
	}
	return float64(s.Cached) / float64(total)
}

// usageStats 持有所有 alias 的 token 用量统计。
type usageStats struct {
	mu   sync.RWMutex
	data map[string]*aliasUsageStats // key = alias
}

func newUsageStats() *usageStats {
	return &usageStats{data: make(map[string]*aliasUsageStats)}
}

// record 异步记录一次请求的 token 用量与费用到指定 alias。
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
	// 费用使用 float64 原子加减(近似安全,不做精确累加)
	s.InputCost += u.InputCost
	s.OutputCost += u.OutputCost
	s.TotalCost += u.TotalCost
}

// recordError 异步记录一次错误请求(4xx/5xx)到指定 alias。
func (us *usageStats) recordError(alias string) {
	if alias == "" || alias == "-" {
		return
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	s := us.data[alias]
	if s == nil {
		s = &aliasUsageStats{}
		us.data[alias] = s
	}
	atomic.AddInt64(&s.Errors, 1)
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
	b.WriteString(`<tr><th>Alias</th><th>请求数</th><th>错误</th><th>输入</th><th>缓存命中</th><th>输出</th><th>命中率</th><th>输入费用</th><th>输出费用</th><th>总费用</th></tr>`)

	var totalPrompt, totalCached, totalCompletion, totalErrors int64
	var totalInputCost, totalOutputCost, totalCost float64
	var totalCount int64
	for _, alias := range aliases {
		s := snap[alias]
		rate := s.cacheHitRate()
		displayRate := rate
		if displayRate > 1 {
			displayRate = 1
		}
		barLen := 20
		filled := int(displayRate * float64(barLen))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
		errDisplay := "-"
		if s.Errors > 0 {
			errDisplay = fmt.Sprintf(`<span style="color:red">%d</span>`, s.Errors)
		}
		fmt.Fprintf(&b, `<tr><td><b>%s</b></td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
			alias, s.Count, errDisplay, fmtTokens(s.Prompt), fmtTokens(s.Cached), fmtTokens(s.Completion))
		fmt.Fprintf(&b, `<td><span style="font-family:monospace;font-size:11px">%s</span> %.1f%%</td>`,
			bar, displayRate*100)
		fmt.Fprintf(&b, `<td>%.4f</td><td>%.4f</td><td><b>%.4f</b></td></tr>`,
			s.InputCost, s.OutputCost, s.TotalCost)
		totalPrompt += s.Prompt
		totalCached += s.Cached
		totalCompletion += s.Completion
		totalInputCost += s.InputCost
		totalOutputCost += s.OutputCost
		totalCost += s.TotalCost
		totalCount += s.Count
		totalErrors += s.Errors
	}

	// 合计行
	var totalRate float64
	if totalPrompt > 0 {
		totalRate = float64(totalCached) / float64(totalPrompt)
	}
	if totalRate > 1 {
		totalRate = 1
	}
	totalErrDisplay := "-"
	if totalErrors > 0 {
		totalErrDisplay = fmt.Sprintf(`<span style="color:red">%d</span>`, totalErrors)
	}
	fmt.Fprintf(&b, `<tr style="font-weight:bold;background:#eee"><td>合计</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
		totalCount, totalErrDisplay, fmtTokens(totalPrompt), fmtTokens(totalCached), fmtTokens(totalCompletion))
	fmt.Fprintf(&b, `<td>%.1f%%</td>`, totalRate*100)
	fmt.Fprintf(&b, `<td>%.4f</td><td>%.4f</td><td>%.4f</td></tr>`, totalInputCost, totalOutputCost, totalCost)
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

// ---------- 持久化 --------------------------------------------------------
//
// 把 token 用量 + 费用统计存到 JSON 文件,重启时读回。
// 与 stats.go 的持久化采用相同的原子写策略(先写 tmp 再 rename)。

// usagePersistSnapshot 是 usage 落盘的文件结构。
// 版本号便于将来升级格式。字段命名与 aliasUsageStats 的 JSON tag 对齐。
type usagePersistSnapshot struct {
	Version int                        `json:"version"`
	Data    map[string]aliasUsageStats `json:"data"`
}

// load 从 path 读取 usage 统计快照,恢复到 usageStats。文件不存在视为空,不报错。
func (us *usageStats) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动,没文件很正常
		}
		return err
	}
	var snap usagePersistSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	// aliasUsageStats 是值类型,直接赋值即可
	for alias, s := range snap.Data {
		// 复制一份再存入 map(避免外部引用)
		cp := s
		us.data[alias] = &cp
	}
	return nil
}

// save 把当前 usage 统计原子写入 path。
func (us *usageStats) save(path string) error {
	snap := us.snapshot() // 已深拷贝,不持锁
	out := usagePersistSnapshot{Version: 1, Data: snap}
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
func (us *usageStats) startPersistLoop(path string, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if err := us.save(path); err != nil {
				log.Printf("usage 统计落盘失败: %v", err)
			}
		}
	}()
}
