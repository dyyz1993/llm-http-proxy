package main

import (
	"strings"
	"testing"
)

// TestExtractUsageOpenAI 验证 OpenAI 格式的 usage 提取。
func TestExtractUsageOpenAI(t *testing.T) {
	body := []byte(`{
		"choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 331,
			"completion_tokens": 20,
			"prompt_tokens_details": {"cached_tokens": 329},
			"completion_tokens_details": {"reasoning_tokens": 19},
			"total_tokens": 351
		}
	}`)
	u := extractUsage(body)
	if !u.HasData {
		t.Fatal("应提取到 usage")
	}
	if u.Prompt != 331 {
		t.Errorf("Prompt = %d, want 331", u.Prompt)
	}
	if u.Cached != 329 {
		t.Errorf("Cached = %d, want 329", u.Cached)
	}
	if u.Completion != 20 {
		t.Errorf("Completion = %d, want 20", u.Completion)
	}
}

// TestExtractUsageAnthropic 验证 Anthropic 格式的 usage 提取。
func TestExtractUsageAnthropic(t *testing.T) {
	body := []byte(`{
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "ok"}],
		"usage": {
			"input_tokens": 6,
			"output_tokens": 2,
			"cache_read_input_tokens": 2
		}
	}`)
	u := extractUsage(body)
	if !u.HasData {
		t.Fatal("应提取到 usage")
	}
	if u.Prompt != 6 {
		t.Errorf("Prompt = %d, want 6", u.Prompt)
	}
	if u.Cached != 2 {
		t.Errorf("Cached = %d, want 2", u.Cached)
	}
	if u.Completion != 2 {
		t.Errorf("Completion = %d, want 2", u.Completion)
	}
}

// TestExtractUsageSSE 验证 SSE 流式响应:从多个 chunk 里找最后一个含 usage 的。
func TestExtractUsageSSE(t *testing.T) {
	// 模拟 OpenAI SSE 流:多个 chunk,最后一个带 usage
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n" +
		"data: [DONE]\n\n"

	u := extractUsage([]byte(sse))
	if !u.HasData {
		t.Fatal("SSE 应从最后一个 chunk 提取到 usage")
	}
	if u.Prompt != 100 {
		t.Errorf("Prompt = %d, want 100", u.Prompt)
	}
	if u.Cached != 80 {
		t.Errorf("Cached = %d, want 80", u.Cached)
	}
}

// TestExtractUsageAnthropicSSE 验证 Anthropic SSE(message_start + message_delta)。
func TestExtractUsageAnthropicSSE(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":300,\"cache_read_input_tokens\":250}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"

	u := extractUsage([]byte(sse))
	if !u.HasData {
		t.Fatal("Anthropic SSE 应提取到 usage")
	}
	// 应取到最后一个含 usage 的 chunk(message_start 有 input+cache)
	if u.Prompt != 300 {
		t.Errorf("Prompt = %d, want 300", u.Prompt)
	}
	if u.Cached != 250 {
		t.Errorf("Cached = %d, want 250", u.Cached)
	}
}

// TestExtractUsageNoUsage 验证不含 usage 的响应返回 HasData=false。
func TestExtractUsageNoUsage(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	u := extractUsage(body)
	if u.HasData {
		t.Error("不含 usage 的响应应返回 HasData=false")
	}

	// 错误响应也不应提取到
	errBody := []byte(`{"error":{"code":"1113","message":"Insufficient balance"}}`)
	u = extractUsage(errBody)
	if u.HasData {
		t.Error("错误响应不应提取到 usage")
	}
}

// TestCacheHitRate 验证缓存命中率计算。
func TestCacheHitRate(t *testing.T) {
	s := aliasUsageStats{Prompt: 1000, Cached: 800, Completion: 200}
	rate := s.cacheHitRate()
	if rate < 0.79 || rate > 0.81 {
		t.Errorf("命中率 = %.2f, want ~0.80", rate)
	}

	// prompt=0 时不除零
	zero := aliasUsageStats{}
	if zero.cacheHitRate() != 0 {
		t.Error("prompt=0 时命中率应为 0")
	}
}

// TestUsageStatsRecord 验证按 alias 聚合 + 异步记录。
func TestUsageStatsRecord(t *testing.T) {
	us := newUsageStats()

	// 记录两次同一 alias
	us.record("glm", usageData{HasData: true, Prompt: 100, Cached: 80, Completion: 20})
	us.record("glm", usageData{HasData: true, Prompt: 200, Cached: 150, Completion: 30})

	snap := us.snapshot()
	s, ok := snap["glm"]
	if !ok {
		t.Fatal("应找到 glm 的统计")
	}
	if s.Prompt != 300 {
		t.Errorf("累计 Prompt = %d, want 300", s.Prompt)
	}
	if s.Cached != 230 {
		t.Errorf("累计 Cached = %d, want 230", s.Cached)
	}
	if s.Completion != 50 {
		t.Errorf("累计 Completion = %d, want 50", s.Completion)
	}
	if s.Count != 2 {
		t.Errorf("Count = %d, want 2", s.Count)
	}

	// 命中率 = 230/300 ≈ 0.767
	rate := s.cacheHitRate()
	if rate < 0.76 || rate > 0.77 {
		t.Errorf("平均命中率 = %.3f, want ~0.767", rate)
	}
}

// TestBuildUsageHTMLHighCache 验证 cached > prompt 时进度条不 panic。
// 回归 bug: z.ai 返回的 cached_tokens 可能远大于 prompt_tokens,
// 导致 cacheHitRate > 1.0,进度条 filled > barLen,strings.Repeat 收到负数 panic。
func TestBuildUsageHTMLHighCache(t *testing.T) {
	// cached 是 prompt 的 232 倍(真实线上数据)
	snap := map[string]aliasUsageStats{
		"max-0": {Prompt: 1432, Cached: 331904, Completion: 88, Count: 5},
		"glm":   {Prompt: 1000, Cached: 800, Completion: 50, Count: 3},
	}
	// 不应 panic
	assertNotPanic(t, func() {
		_ = buildUsageHTML(snap)
	})

	html := buildUsageHTML(snap)
	if !strings.Contains(html, "max-0") {
		t.Error("应包含 max-0 alias")
	}
	if !strings.Contains(html, "合计") {
		t.Error("应包含合计行")
	}
}
func TestUsageStatsIgnoreEmpty(t *testing.T) {
	us := newUsageStats()
	// HasData=false 不应记录
	us.record("glm", usageData{HasData: false})
	us.record("", usageData{HasData: true, Prompt: 10}) // alias 空也不记

	if len(us.snapshot()) != 0 {
		t.Error("空数据/空 alias 不应被记录")
	}
}

// TestExtractUsageTruncation 验证超大 SSE body 截断后仍能提取。
// 超大 body 通常出现在 SSE 流(很多 chunk),截断后保留末尾 512KB。
func TestExtractUsageTruncation(t *testing.T) {
	// 构造一个 >2MB 的 SSE 流:很多 chunk + 末尾带 usage
	var sb strings.Builder
	// 填充 3MB 的 chunk
	for i := 0; i < 40000; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"padding\"}}]}\n\n")
	}
	// 末尾放 usage
	sb.WriteString("data: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	body := []byte(sb.String())

	u := extractUsage(body)
	if !u.HasData {
		t.Fatal("超大 SSE body 截断后仍应提取到 usage")
	}
	if u.Prompt != 50 {
		t.Errorf("Prompt = %d, want 50", u.Prompt)
	}
}
