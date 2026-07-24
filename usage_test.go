package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	// Anthropic: input_tokens 是新增部分,总输入 = input + cache_read = 6 + 2 = 8
	if u.Prompt != 8 {
		t.Errorf("Prompt = %d, want 8 (input+cache_read)", u.Prompt)
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
	// Anthropic: 总输入 = input_tokens(300) + cache_read(250) = 550
	if u.Prompt != 550 {
		t.Errorf("Prompt = %d, want 550 (input+cache_read)", u.Prompt)
	}
	if u.Cached != 250 {
		t.Errorf("Cached = %d, want 250", u.Cached)
	}
	if u.Completion != 10 {
		t.Errorf("Completion = %d, want 10", u.Completion)
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

// TestCacheHitRate 验证缓存命中率计算 = cached / (prompt + cached)。
func TestCacheHitRate(t *testing.T) {
	// 正常情况:prompt=1000, cached=800 → 800/1800 ≈ 44.4%
	s := aliasUsageStats{Prompt: 1000, Cached: 800, Completion: 200}
	rate := s.cacheHitRate()
	if rate < 0.44 || rate > 0.45 {
		t.Errorf("命中率 = %.4f, want ~0.444", rate)
	}

	// 线上真实数据:prompt=37, cached=333248 → 99.99%(不会超过 100%)
	extreme := aliasUsageStats{Prompt: 37, Cached: 333248}
	extremeRate := extreme.cacheHitRate()
	if extremeRate > 1.0 {
		t.Errorf("命中率不应超过 100%%, got %.4f", extremeRate)
	}
	if extremeRate < 0.9998 {
		t.Errorf("极高缓存率应为 ~99.99%%, got %.4f", extremeRate)
	}

	// prompt=0 时不除零
	zero := aliasUsageStats{}
	if zero.cacheHitRate() != 0 {
		t.Error("prompt=0 时命中率应为 0")
	}

	// 全部命中:prompt=0, cached=100 → 100%
	allCached := aliasUsageStats{Cached: 100}
	if allCached.cacheHitRate() != 1.0 {
		t.Errorf("全命中应为 100%%, got %.4f", allCached.cacheHitRate())
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

	// 命中率 = 230/(300+230) ≈ 0.434
	rate := s.cacheHitRate()
	if rate < 0.43 || rate > 0.44 {
		t.Errorf("平均命中率 = %.3f, want ~0.434", rate)
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
		_ = buildUsageHTML(snap, nil, nil)
	})

	html := buildUsageHTML(snap, nil, nil)
	if !strings.Contains(html, "max-0") {
		t.Error("应包含 max-0 alias")
	}
	if !strings.Contains(html, "合计") {
		t.Error("应包含合计行")
	}
	// 命中率应截断到 100%,不应出现 900670% 这种
	if strings.Contains(html, "900670") {
		t.Error("命中率应截断到 100%,不应显示超大百分比")
	}
}

// TestFmtTokens 验证 token 数量换算。
func TestFmtTokens(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{333248, "333.2K"},
		{1000000, "1.0M"},
		{2500000, "2.5M"},
	}
	for _, c := range cases {
		if got := fmtTokens(c.input); got != c.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", c.input, got, c.want)
		}
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

// ---------- 持久化测试 ----------

// TestExtractSSEModelSeparateChunk 验证 SSE 流里 model 和 usage 分散在不同 chunk 时,
// model 字段能被正确合并(不丢失)。
// 这是 v2.4.1 修复的 bug:SSE 的 model 在第一个 chunk,usage 在最后一个 chunk,
// 合并时漏了 Model 字段导致费用算不出。
func TestExtractSSEModelSeparateChunk(t *testing.T) {
	// 模拟真实 GLM SSE 响应:
	// - 第一个 chunk 含 model(但无 usage)
	// - 中间 chunk 是增量内容(无 model 无 usage)
	// - 最后一个 chunk 含 usage(但可能无 model)
	body := []byte(`data: {"id":"123","model":"GLM-4.7","choices":[{"delta":{"content":""}}]}

data: {"id":"123","choices":[{"delta":{"content":"你好"}}]}

data: {"id":"123","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":223,"total_tokens":229}}

data: [DONE]

`)
	u := extractUsage(body)
	if !u.HasData {
		t.Fatal("应提取到 usage")
	}
	if u.Model == "" {
		t.Fatal("Model 应该被提取到(model 在第一个 chunk,usage 在最后一个),但得到空字符串 — 这是 v2.4.0 的 bug")
	}
	if u.Model != "GLM-4.7" && !strings.EqualFold(u.Model, "glm-4.7") {
		t.Errorf("Model = %q, want GLM-4.7", u.Model)
	}
	if u.Prompt != 6 {
		t.Errorf("Prompt = %d, want 6", u.Prompt)
	}
	if u.Completion != 223 {
		t.Errorf("Completion = %d, want 223", u.Completion)
	}
}

// TestExtractUsageModelCaseInsensitive 验证大小写不影响费用计算。
// 官方返回 "GLM-4.7",用户可能配 "glm-4.7",都应正确匹配。
func TestExtractUsageModelCaseInsensitive(t *testing.T) {
	tests := []string{"GLM-4.7", "glm-4.7", "Glm-4.7", "GLM-4.7-Flash"}
	for _, model := range tests {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","usage":{"prompt_tokens":100,"completion_tokens":50}}`)
			u := extractUsage(body)
			if !u.HasData {
				t.Fatal("应提取到 usage")
			}
			if u.Model != model {
				t.Errorf("Model = %q, want %q", u.Model, model)
			}
		})
	}
}

// TestSimulateProxySlidingWindow 模拟代理的滑动窗口逻辑 + SSE 提取。
// 这是线上 v2.4.5 "流式费用算不出" 问题的本地复现测试。
// 完整模拟:第一个 chunk 含 model → 大量 reasoning chunk → 最后 chunk 含 usage。
// 滑动窗口只保留 512KB + 第一个 chunk,拼接后 extractUsage 必须能提取出 model + usage。
func TestSimulateProxySlidingWindow(t *testing.T) {
	tests := []struct {
		name            string
		reasoningChunks int // 中间 reasoning chunk 数量
		chunkSize       int // 每个 reasoning chunk 大小
		wantModel       string
		wantPrompt      int64
		wantCompletion  int64
	}{
		{"小响应(10个chunk)", 10, 500, "glm-4.7", 6, 100},
		{"中等响应(100个chunk,~50KB)", 100, 500, "glm-4.7", 6, 100},
		{"大响应(1000个chunk,~500KB)", 1000, 500, "glm-4.7", 6, 100},
		{"超大响应(5000个chunk,~2.5MB)", 5000, 500, "glm-4.7", 6, 100},
		{"超大响应(1MB chunk × 10)", 10, 1024 * 1024, "glm-4.7", 6, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body strings.Builder
			// 第一个 chunk:含 model
			body.WriteString("data: " + fmt.Sprintf(`{"id":"1","model":"%s","choices":[{"delta":{"content":""}}]}`, tt.wantModel) + "\n\n")
			// 中间 chunks:reasoning
			reasoning := strings.Repeat("x", tt.chunkSize)
			for i := 0; i < tt.reasoningChunks; i++ {
				body.WriteString("data: " + fmt.Sprintf(`{"id":"1","choices":[{"delta":{"reasoning_content":"%s"}}]}`, reasoning) + "\n\n")
			}
			// 最后 chunk:含 usage
			body.WriteString("data: " + fmt.Sprintf(`{"id":"1","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, tt.wantPrompt, tt.wantCompletion) + "\n\n")
			body.WriteString("data: [DONE]\n\n")

			// 模拟代理滑动窗口
			slidingWindow := 512 * 1024
			var captureBuf []byte
			var sseFirstChunk []byte
			rawBody := body.String()
			chunks := strings.SplitAfter(rawBody, "\n\n")
			chunkCount := 0
			for _, chunk := range chunks {
				if chunk == "" {
					continue
				}
				if chunkCount == 0 {
					sseFirstChunk = append(sseFirstChunk, []byte(chunk)...)
				}
				captureBuf = append(captureBuf, []byte(chunk)...)
				if len(captureBuf) > slidingWindow {
					captureBuf = captureBuf[len(captureBuf)-slidingWindow:]
				}
				chunkCount++
			}
			if len(sseFirstChunk) > 0 {
				captureBuf = append(sseFirstChunk, captureBuf...)
			}

			u := extractUsage(captureBuf)
			if !u.HasData {
				t.Fatalf("HasData=false (bodyLen=%d)", len(captureBuf))
			}
			if !strings.EqualFold(u.Model, tt.wantModel) {
				t.Errorf("Model=%q want %q", u.Model, tt.wantModel)
			}
			if u.Prompt != tt.wantPrompt {
				t.Errorf("Prompt=%d want %d", u.Prompt, tt.wantPrompt)
			}
			if u.Completion != tt.wantCompletion {
				t.Errorf("Completion=%d want %d", u.Completion, tt.wantCompletion)
			}
		})
	}
}

// ---------- 持久化测试 ----------

// TestUsagePersistRoundTrip 验证 save → 新实例 load → 数据完全一致。
// 覆盖 token 用量 + 费用 + 缓存命中 + 多 alias。
func TestUsagePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	// 写入若干 alias 的统计(token + 费用 + 缓存)
	us := newUsageStats()
	us.record("glm", usageData{HasData: true, Model: "glm-4.7", Prompt: 1500, Cached: 500, Completion: 200, CostCalculated: true, InputCost: 0.003, OutputCost: 0.004, TotalCost: 0.007})
	us.record("glm", usageData{HasData: true, Model: "glm-5.1", Prompt: 3000, Completion: 100, CostCalculated: true, InputCost: 0.02, OutputCost: 0.01, TotalCost: 0.03})
	us.record("claude", usageData{HasData: true, Model: "claude-3", Prompt: 800, Cached: 100, Completion: 50, CostCalculated: false})

	// 保存
	if err := us.save(path); err != nil {
		t.Fatalf("save 失败: %v", err)
	}

	// 用新实例读回
	us2 := newUsageStats()
	if err := us2.load(path); err != nil {
		t.Fatalf("load 失败: %v", err)
	}

	// 比较快照
	orig := us.snapshot()
	loaded := us2.snapshot()
	if len(orig) != len(loaded) {
		t.Fatalf("alias 数量不一致: orig=%d loaded=%d", len(orig), len(loaded))
	}
	for alias, want := range orig {
		got, ok := loaded[alias]
		if !ok {
			t.Errorf("load 后缺少 alias %q", alias)
			continue
		}
		if got.Prompt != want.Prompt {
			t.Errorf("[%s] Prompt: got %d, want %d", alias, got.Prompt, want.Prompt)
		}
		if got.Cached != want.Cached {
			t.Errorf("[%s] Cached: got %d, want %d", alias, got.Cached, want.Cached)
		}
		if got.Completion != want.Completion {
			t.Errorf("[%s] Completion: got %d, want %d", alias, got.Completion, want.Completion)
		}
		if got.Count != want.Count {
			t.Errorf("[%s] Count: got %d, want %d", alias, got.Count, want.Count)
		}
		if got.InputCost != want.InputCost {
			t.Errorf("[%s] InputCost: got %f, want %f", alias, got.InputCost, want.InputCost)
		}
		if got.OutputCost != want.OutputCost {
			t.Errorf("[%s] OutputCost: got %f, want %f", alias, got.OutputCost, want.OutputCost)
		}
		if got.TotalCost != want.TotalCost {
			t.Errorf("[%s] TotalCost: got %f, want %f", alias, got.TotalCost, want.TotalCost)
		}
	}
}

// TestUsagePersistLoadMissingFile 验证文件不存在时 load 不报错。
// 首次启动时没有文件是正常情况。
func TestUsagePersistLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	us := newUsageStats()
	if err := us.load(path); err != nil {
		t.Errorf("文件不存在时 load 应返回 nil,但得到: %v", err)
	}
	if len(us.snapshot()) != 0 {
		t.Error("load 不存在的文件后应该没有数据")
	}
}

// TestUsagePersistOverwrite 验证多次 save 会覆盖旧文件(不是追加)。
// 防止统计回退后旧数据残留。
func TestUsagePersistOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	// 第一次写
	us1 := newUsageStats()
	us1.record("glm", usageData{HasData: true, Prompt: 100, Completion: 10, TotalCost: 0.001})
	if err := us1.save(path); err != nil {
		t.Fatal(err)
	}
	info1, _ := os.Stat(path)
	if info1.Size() == 0 {
		t.Fatal("第一次 save 文件大小不应为 0")
	}

	// 第二次写(新实例,只记一条不同的数据)
	us2 := newUsageStats()
	us2.record("glm", usageData{HasData: true, Prompt: 50, Completion: 5, TotalCost: 0.0005})
	if err := us2.save(path); err != nil {
		t.Fatal(err)
	}

	// 读回验证:应该是第二次的数据(50/5),不是第一次(100/10)
	us3 := newUsageStats()
	us3.load(path)
	got := us3.snapshot()["glm"]
	if got.Prompt != 50 {
		t.Errorf("覆盖后 Prompt = %d, want 50", got.Prompt)
	}
	if got.Completion != 5 {
		t.Errorf("覆盖后 Completion = %d, want 5", got.Completion)
	}
	if got.TotalCost != 0.0005 {
		t.Errorf("覆盖后 TotalCost = %f, want 0.0005", got.TotalCost)
	}
}

// TestProxyNonStreamUsageCapture 端到端验证:非流式(普通 JSON)GLM 响应
// 经过代理后,usage 是否被正确捕获并记录到 usageTracker。
// 这是排查"线上非流式统计不到 token"的关键测试。
func TestProxyNonStreamUsageCapture(t *testing.T) {
	// 后端:模拟真实非流式 GLM 响应(含 usage + cached_tokens)
	glmResp := `{
		"id":"123","object":"chat.completion","model":"glm-4.7",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1500,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":500}}
	}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, glmResp)
	}))
	defer backend.Close()

	// 临时设置包级 usageTracker(模拟 main() 初始化),测试后还原
	oldTracker := usageTracker
	usageTracker = newUsageStats()
	defer func() { usageTracker = oldTracker }()

	stats := newStatsCollector()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		newProxyHandler(stats, nil, "", nil, nil, RetryConfig{}).ServeHTTP(w, req)
	}))
	defer proxy.Close()

	// 用 Authorization 走透传模式(statKey = key:xxxx)
	req, _ := http.NewRequest("POST",
		proxyURL(proxy.URL, backend.URL+"/v4/chat/completions"),
		strings.NewReader(`{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-abcd1234efgh5678")
	req.Header.Set("Content-Type", "application/json")
	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 给后台解析一点时间(record 在响应结束后同步调用,但保险起见)
	time.Sleep(50 * time.Millisecond)

	// 检查 usageTracker 有没有记录到
	snap := usageTracker.snapshot()
	t.Logf("snapshot: %+v", snap)
	if len(snap) == 0 {
		t.Fatalf("usageTracker 为空,非流式 usage 未被捕获")
	}
	// 透传模式 alias = 掩码 key
	var s aliasUsageStats
	for _, v := range snap {
		s = v
		break
	}
	if s.Prompt != 1500 {
		t.Errorf("Prompt = %d, want 1500", s.Prompt)
	}
	if s.Cached != 500 {
		t.Errorf("Cached = %d, want 500", s.Cached)
	}
	if s.Completion != 80 {
		t.Errorf("Completion = %d, want 80", s.Completion)
	}
	if s.Count != 1 {
		t.Errorf("Count = %d, want 1", s.Count)
	}
}

// TestExtractAnthropicSSEModel 验证 Anthropic 格式 SSE 能提取嵌套的 model。
// 回归 bug: Anthropic SSE 的 model 在 message.model 里(嵌套),
// 之前 trySSE 只解顶层 model → model 为空 → 费用算不出。
// 导致走 api.z.ai/api/anthropic 路径的请求全部没费用。
func TestExtractAnthropicSSEModel(t *testing.T) {
	body := []byte(`event: message_start
data: {"type": "message_start", "message": {"id": "msg_123", "type": "message", "role": "assistant", "model": "glm-4.6", "content": [], "usage": {"input_tokens": 0, "output_tokens": 0}}}

event: content_block_delta
data: {"type": "content_block_delta", "delta": {"type": "text_delta", "text": "Hi"}}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 5}}

event: message_stop
data: {"type": "message_stop"}
`)
	u := extractUsage(body)
	if u.Model != "glm-4.6" {
		t.Errorf("Model = %q, want glm-4.6 (Anthropic 嵌套 message.model)", u.Model)
	}
}

// TestExtractOpenAISSEModelStillafterFix 验证 OpenAI 格式(顶层 model)修复后仍正常。
func TestExtractOpenAISSEModelStillafterFix(t *testing.T) {
	body := []byte(`data: {"id":"1","model":"glm-4.6","choices":[{"delta":{"content":"hi"}}]}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}

data: [DONE]
`)
	u := extractUsage(body)
	if u.Model != "glm-4.6" {
		t.Errorf("OpenAI SSE Model = %q, want glm-4.6", u.Model)
	}
}

// --- Quota / 限额 测试 ---

// TestCheckQuotaNoLimit 验证未配置限额时永远允许。
func TestCheckQuotaNoLimit(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{Key: "sk-test"} // HasQuota() == false

	ok, reason, _ := us.checkQuota("test-alias", cfg)
	if !ok {
		t.Errorf("未配置限额时应允许, 但被拒绝: %s", reason)
	}
}

// TestCheckQuotaUnderLimit 验证未超限时允许。
func TestCheckQuotaUnderLimit(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 10, MaxTokens: 1000}

	// 先记录一点用量
	us.recordSuccess("test-alias")
	us.record("test-alias", usageData{HasData: true, Prompt: 50, Completion: 30})

	ok, reason, _ := us.checkQuota("test-alias", cfg)
	if !ok {
		t.Errorf("未超限时应允许, 但被拒绝: %s", reason)
	}
}

// TestCheckQuotaReqsExceeded 验证请求次数超限时拒绝。
func TestCheckQuotaReqsExceeded(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 5, Window: "1h"}

	// 已达上限
	for i := int64(0); i < 5; i++ {
		us.recordSuccess("test-alias")
	}

	ok, reason, retryAfter := us.checkQuota("test-alias", cfg)
	if ok {
		t.Fatal("请求次数超限时应拒绝, 但被允许")
	}
	if retryAfter <= 0 {
		t.Errorf("应返回 retryAfter > 0, 但 got %v", retryAfter)
	}
	t.Logf("拒绝原因: %s, retryAfter: %v", reason, retryAfter)
}

// TestCheckQuotaTokensExceeded 验证 token 用量超限时拒绝。
func TestCheckQuotaTokensExceeded(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxTokens: 100, Window: "1h"}

	// 已达上限
	us.record("test-alias", usageData{HasData: true, Prompt: 60, Completion: 40})
	us.recordSuccess("test-alias")

	ok, reason, retryAfter := us.checkQuota("test-alias", cfg)
	if ok {
		t.Fatal("用量超限时应拒绝, 但被允许")
	}
	if retryAfter <= 0 {
		t.Errorf("应返回 retryAfter > 0, 但 got %v", retryAfter)
	}
	t.Logf("拒绝原因: %s, retryAfter: %v", reason, retryAfter)
}

// TestCheckQuotaHardLimit 验证硬上限(默认 100d 窗口)不自动重置。
func TestCheckQuotaHardLimit(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 3} // 无 window, 默认 100 天

	for i := int64(0); i < 3; i++ {
		us.recordSuccess("test-alias")
	}

	ok, _, _ := us.checkQuota("test-alias", cfg)
	if ok {
		t.Fatal("硬上限超限时应拒绝, 但被允许")
	}
}

// TestCheckQuotaWindowReset 验证窗口过期后自动重置。
func TestCheckQuotaWindowReset(t *testing.T) {
	us := newUsageStats()
	// 用一个极短的窗口(10ms 就过期)
	cfg := KeyConfig{MaxReqs: 2, Window: "10ms"}

	// 填满窗口
	us.recordSuccess("test-alias")
	us.recordSuccess("test-alias")

	ok, _, _ := us.checkQuota("test-alias", cfg)
	if ok {
		t.Fatal("窗口未过期时应拒绝, 但被允许")
	}

	// 等窗口过期
	time.Sleep(15 * time.Millisecond)

	ok, reason, _ := us.checkQuota("test-alias", cfg)
	if !ok {
		t.Errorf("窗口过期后应自动重置并允许, 但被拒绝: %s", reason)
	}
}

// TestCheckQuotaMixedLimits 验证同时配置两种限额时,先达到的优先拒绝。
func TestCheckQuotaMixedLimits(t *testing.T) {
	us := newUsageStats()
	// 请求上限 3, token 上限 100。先达 token 上限。
	cfg := KeyConfig{MaxReqs: 3, MaxTokens: 100}

	us.record("test-alias", usageData{HasData: true, Prompt: 80, Completion: 30})
	us.recordSuccess("test-alias")

	ok, _, _ := us.checkQuota("test-alias", cfg)
	if ok {
		t.Fatal("token 超限时应拒绝, 但被允许")
	}
}

// TestRecordSuccess 验证 recordSuccess 正确累加窗口计数器。
func TestRecordSuccess(t *testing.T) {
	us := newUsageStats()

	us.recordSuccess("test-alias")
	us.recordSuccess("test-alias")
	us.recordSuccess("test-alias")

	snap := us.snapshot()
	s, ok := snap["test-alias"]
	if !ok {
		t.Fatal("应存在 test-alias")
	}
	if s.WindowSuccess != 3 {
		t.Errorf("WindowSuccess = %d, want 3", s.WindowSuccess)
	}
}

// ---------- 窗口时长解析 / 友好格式 / recordError / 边界测试 ----------

// TestParseWindowDuration 验证 parseWindowDuration 各种格式。
func TestParseWindowDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"", 0, true}, // 空 → 报错
		{"5h", 5 * time.Hour, false},
		{"1h", 1 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"100d", 100 * 24 * time.Hour, false},
		{"1d", 1 * 24 * time.Hour, false},
		{"100000d", 100000 * 24 * time.Hour, false},
		{"abc", 0, true},
		{"1.5h", 90 * time.Minute, false}, // Go time.ParseDuration 支持小数
		{"-5h", -5 * time.Hour, false},    // Go time.ParseDuration 支持负数
		{"0d", 0, false},                  // 0 days is valid
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseWindowDuration(tt.input)
			if tt.err {
				if err == nil {
					t.Errorf("parseWindowDuration(%q) 应报错, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseWindowDuration(%q) 不应报错: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseWindowDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestWindowDurationDefault 验证 WindowDuration() 的默认值和错误回退。
func TestWindowDurationDefault(t *testing.T) {
	defaultDur := 100 * 24 * time.Hour

	// 空 Window → 默认 100 天
	if d := (KeyConfig{}).WindowDuration(); d != defaultDur {
		t.Errorf("空 Window = %v, want %v", d, defaultDur)
	}
	// Window="invalid" → 默认 100 天
	if d := (KeyConfig{Window: "invalid"}).WindowDuration(); d != defaultDur {
		t.Errorf("无效 Window = %v, want %v", d, defaultDur)
	}
	// Window="5h" → 5h
	if d := (KeyConfig{Window: "5h"}).WindowDuration(); d != 5*time.Hour {
		t.Errorf("5h = %v, want %v", d, 5*time.Hour)
	}
}

// TestFriendlyDuration 验证友好时长格式(中文输出)。
func TestFriendlyDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0分钟"},
		{30 * time.Minute, "30分钟"},
		{1 * time.Hour, "1小时"},
		{2*time.Hour + 30*time.Minute, "2小时30分钟"},
		{25 * time.Hour, "1天1小时"},
		{7 * 24 * time.Hour, "7天"},
		{100 * 24 * time.Hour, "100天"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := friendlyDuration(tt.d); got != tt.want {
				t.Errorf("friendlyDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// TestRecordError 验证 recordError 正确累加错误计数。
func TestRecordError(t *testing.T) {
	us := newUsageStats()

	// 空 alias/空 statKey → 忽略
	us.recordError("")
	us.recordError("-")

	us.recordError("glm")
	us.recordError("glm")
	us.recordError("claude")

	snap := us.snapshot()
	if g, ok := snap["glm"]; !ok {
		t.Fatal("glm 应存在")
	} else if g.Errors != 2 {
		t.Errorf("glm Errors = %d, want 2", g.Errors)
	}
	if g, ok := snap["claude"]; !ok {
		t.Fatal("claude 应存在")
	} else if g.Errors != 1 {
		t.Errorf("claude Errors = %d, want 1", g.Errors)
	}
	// 空 alias 不应产生条目
	if len(snap) != 2 {
		t.Errorf("snapshot 条目数 = %d, want 2 (空 alias 不应创建条目)", len(snap))
	}
}

// TestRecordErrorAfterRecord 验证 record 后 recordError 不丢失统计数据。
func TestRecordErrorAfterRecord(t *testing.T) {
	us := newUsageStats()
	us.record("glm", usageData{HasData: true, Prompt: 100, Completion: 50})
	us.recordError("glm")

	snap := us.snapshot()
	s := snap["glm"]
	if s.Prompt != 100 {
		t.Errorf("Prompt = %d, want 100", s.Prompt)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.Errors)
	}
}

// TestCheckQuotaEdgeCase_ZeroLimits 验证 HasQuota=false 时不检查。
func TestCheckQuotaEdgeCase_ZeroLimits(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 0, MaxTokens: 0} // HasQuota=false

	ok, reason, _ := us.checkQuota("test", cfg)
	if !ok {
		t.Errorf("0 限额应允许, 但被拒绝: %s", reason)
	}
}

// TestCheckQuotaEdgeCase_NoRecordBefore 验证从未记录过统计的 alias 自动创建。
func TestCheckQuotaEdgeCase_NoRecordBefore(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 5, Window: "1h"}

	// 从未 record 过,直接 checkQuota → 应自动创建条目,允许通过
	ok, reason, _ := us.checkQuota("new-alias", cfg)
	if !ok {
		t.Errorf("新 alias 应自动创建并允许, 但被拒绝: %s", reason)
	}

	// snapshot 里应包含新 alias
	snap := us.snapshot()
	if _, exists := snap["new-alias"]; !exists {
		t.Error("checkQuota 后 snapshot 应包含 new-alias")
	}
}

// TestCheckQuota_SuccessCountDoesntAffectTokenLimit 验证成功计数不影响 token 限额。
func TestCheckQuota_SuccessCountDoesntAffectTokenLimit(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxTokens: 100}

	// 产生大量成功请求(不消耗 token)
	for i := int64(0); i < 1000; i++ {
		us.recordSuccess("test")
	}

	// token 用量为 0,不应被拒绝
	ok, reason, _ := us.checkQuota("test", cfg)
	if !ok {
		t.Errorf("只有成功计数不应触发 token 限额: %s", reason)
	}
}

// TestCheckQuota_TokensCombo 验证 prompt+cached+completion 合计计入 MaxTokens。
func TestCheckQuota_TokensCombo(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxTokens: 500}

	us.record("test", usageData{HasData: true, Prompt: 200, Cached: 100, Completion: 150})

	ok, _, _ := us.checkQuota("test", cfg)
	if !ok {
		t.Fatal("合计 450 < 500, 应允许")
	}

	// 再来一次,现在合计 > 500
	us.record("test", usageData{HasData: true, Prompt: 100, Cached: 0, Completion: 50})

	ok, reason, _ := us.checkQuota("test", cfg)
	if ok {
		t.Fatal("合计 600 >= 500, 应拒绝")
	}
	t.Logf("拒绝原因: %s", reason)
}

// TestCheckQuota_LongDurationNoOverflow 验证超大窗口不溢出。
func TestCheckQuota_LongDurationNoOverflow(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 1, Window: "100000d"}

	// 填满(1 次成功)
	us.recordSuccess("test")

	// 应被拒绝
	ok, reason, retryAfter := us.checkQuota("test", cfg)
	if ok {
		t.Fatal("100000d 窗口应拒绝超额请求")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter 应 > 0, got %v", retryAfter)
	}
	t.Logf("拒绝原因: %s, retryAfter: %v", reason, retryAfter)
}

// TestCheckQuota_Concurrent 并发调用 checkQuota / recordSuccess / record 无 data race。
func TestCheckQuota_Concurrent(t *testing.T) {
	us := newUsageStats()
	cfg := KeyConfig{MaxReqs: 1000, MaxTokens: 1000000, Window: "1h"}

	var wg sync.WaitGroup
	// 20 个 goroutine 同时读写
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				us.recordSuccess("concurrent")
				us.record("concurrent", usageData{HasData: true, Prompt: 10, Completion: 5})
				us.recordError("concurrent")
				ok, reason, _ := us.checkQuota("concurrent", cfg)
				_ = ok
				_ = reason
			}
		}()
	}
	wg.Wait()

	snap := us.snapshot()
	s := snap["concurrent"]
	// 只是大致验证:20*50=1000 次成功,但可能窗口重置了,只检查不 panic
	t.Logf("并发后: WindowSuccess=%d, Prompt=%d, Errors=%d",
		s.WindowSuccess, s.Prompt, s.Errors)
	if s.Prompt == 0 {
		t.Error("并发后 Prompt 不应为 0")
	}
}

// TestMultiplierPlusQuota 验证 token 乘数(×3) + 额度限额(five_hour_window)同时生效。
// 模拟完整链路: extractUsage → applyTokenMultiplier → record → checkQuota。
func TestMultiplierPlusQuota(t *testing.T) {
	us := newUsageStats()
	// five_hour_window: 5h 窗口, 1 亿 token 上限
	cfg := KeyConfig{
		MaxTokens: 100000000,
		Window:    "5h",
	}

	// token_multipliers: 2:00-6:00 ×3 (用 00:00-23:59 模拟"始终生效")
	rules := []TokenMultiplierRule{
		{
			Multiply:  3.0,
			TimeBlock: &TimeBlock{Start: "00:00", End: "23:59"},
		},
	}

	// 模拟一次请求: 用了 100 输入(含 20 缓存) + 50 输出
	model := "glm-5.1-flash"
	domain := "open.bigmodel.cn"
	promptOrig := int64(100)
	cachedOrig := int64(20)
	completionOrig := int64(50)

	// 第一步: applyTokenMultiplier
	m := applyTokenMultiplierAt(rules, model, domain, parseTime("03:00"))
	if m != 3.0 {
		t.Fatalf("乘数应=3.0, 得到 %.1f", m)
	}

	u := usageData{
		HasData:    true,
		Prompt:     int64(float64(promptOrig) * m),
		Cached:     int64(float64(cachedOrig) * m),
		Completion: int64(float64(completionOrig) * m),
	}
	t.Logf("乘数后: Prompt=%d(原%d), Cached=%d(原%d), Completion=%d(原%d)",
		u.Prompt, promptOrig, u.Cached, cachedOrig, u.Completion, completionOrig)

	// 第二步: record
	us.record("mult-test", u)

	// 第三步: checkQuota → 应允许(300+60+150=510 << 1亿)
	ok, reason, _ := us.checkQuota("mult-test", cfg)
	if !ok {
		t.Fatalf("第一次请求不应超限: %s", reason)
	}
	t.Logf("第一次 checkQuota: 通过")

	// 记录大量请求验证配额追踪
	// 注意: checkQuota 只算 prompt + completion (cached 已包含在 prompt 中)
	tokensPerReq := u.Prompt + u.Completion // 300 + 150 = 450
	for i := 0; i < 50000; i++ {
		us.record("mult-test", u)
	}
	ok, reason, _ = us.checkQuota("mult-test", cfg)
	if !ok {
		t.Fatalf("50000 次后不应超限(%d << 1亿): %s",
			tokensPerReq*50001, reason)
	}
	t.Logf("50000 次后: 通过(累计约 %dM)", (tokensPerReq*50001)/1000000)

	// 再填 250000 次 → 超过 1 亿 (450 * 300001 ≈ 135M)
	for i := 0; i < 250000; i++ {
		us.record("mult-test", u)
	}
	ok, reason, retryAfter := us.checkQuota("mult-test", cfg)
	if ok {
		t.Fatal("超限后应被拒绝")
	}
	t.Logf("超限后拒绝: %s (retryAfter=%v)", reason, retryAfter)
	if retryAfter <= 0 {
		t.Errorf("retryAfter 应 > 0")
	}
	t.Log("✓ 乘数×3 + 5h窗口 同时生效验证通过")
}

// TestMultiplierWithProfile 验证 profile 引用 + 乘数叠加的实际使用场景。
func TestMultiplierWithProfile(t *testing.T) {
	us := newUsageStats()

	// five_hour_window profile
	profiles := map[string]InterceptorProfile{
		"five_hour_window": {
			Window:    "5h",
			MaxTokens: 100000000,
		},
	}

	// alias 配置:引用 five_hour_window profile
	cfg := KeyConfig{
		Key:     "sk-test",
		Profile: "five_hour_window",
	}
	// resolveConfig 合并
	resolved := resolveConfig(cfg, profiles, "")
	if !resolved.HasQuota() {
		t.Fatal("resolveConfig 后应有配额限制")
	}
	if resolved.MaxTokens != 100000000 {
		t.Errorf("MaxTokens 应为 100000000, 得到 %d", resolved.MaxTokens)
	}
	if resolved.Window != "5h" {
		t.Errorf("Window 应为 5h, 得到 %s", resolved.Window)
	}
	t.Logf("resolveConfig: MaxTokens=%d Window=%s", resolved.MaxTokens, resolved.Window)

	// 全局 token_multipliers
	rules := []TokenMultiplierRule{
		{
			Multiply:  3.0,
			TimeBlock: &TimeBlock{Start: "00:00", End: "23:59"},
		},
	}

	model := "glm-5.1-flash"
	domain := "open.bigmodel.cn"
	m := applyTokenMultiplierAt(rules, model, domain, parseTime("03:00"))
	if m != 3.0 {
		t.Fatalf("乘数应=3.0, 得到 %.1f", m)
	}

	u := usageData{
		HasData:    true,
		Prompt:     int64(float64(100) * m), // 300
		Cached:     int64(float64(20) * m),  // 60
		Completion: int64(float64(50) * m),  // 150
	}
	us.record("profile-test", u)

	ok, reason, _ := us.checkQuota("profile-test", resolved)
	if !ok {
		t.Fatalf("引用 profile 后配额检查失败: %s", reason)
	}
	t.Log("✓ profile 引用 + 乘数叠加 同时生效验证通过")
}

// TestDailyTracking 验证每日用量统计正确记录。
func TestDailyTracking(t *testing.T) {
	us := newUsageStats()
	today := time.Now().In(beijing).Format("2006-01-02")

	// 记录第一天的用量
	us.record("test-alias", usageData{HasData: true, Prompt: 1000, Cached: 200, Completion: 500, InputCost: 0.01, OutputCost: 0.02, TotalCost: 0.03})
	us.record("test-alias", usageData{HasData: true, Prompt: 2000, Cached: 300, Completion: 600, InputCost: 0.02, OutputCost: 0.04, TotalCost: 0.06})
	us.recordError("test-alias")

	snap := us.snapshot()
	s := snap["test-alias"]

	// 验证当天数据
	d, ok := s.Daily[today]
	if !ok {
		t.Fatal("当天应有每日统计")
	}
	if d.Prompt != 3000 {
		t.Errorf("当天 Prompt = %d, want 3000", d.Prompt)
	}
	if d.Cached != 500 {
		t.Errorf("当天 Cached = %d, want 500", d.Cached)
	}
	if d.Completion != 1100 {
		t.Errorf("当天 Completion = %d, want 1100", d.Completion)
	}
	if d.Count != 2 {
		t.Errorf("当天 Count = %d, want 2", d.Count)
	}
	if d.Errors != 1 {
		t.Errorf("当天 Errors = %d, want 1", d.Errors)
	}
	if d.InputCost != 0.03 {
		t.Errorf("当天 InputCost = %f, want 0.03", d.InputCost)
	}
	if d.OutputCost != 0.06 {
		t.Errorf("当天 OutputCost = %f, want 0.06", d.OutputCost)
	}
	if d.TotalCost != 0.09 {
		t.Errorf("当天 TotalCost = %f, want 0.09", d.TotalCost)
	}

	// 验证累计统计也正确
	if s.Prompt != 3000 || s.Completion != 1100 {
		t.Errorf("累计统计不正确: Prompt=%d Completion=%d", s.Prompt, s.Completion)
	}
	if s.Errors != 1 {
		t.Errorf("累计错误数 = %d, want 1", s.Errors)
	}

	t.Log("✓ 每日用量统计正确")
}

// TestDailyTracking_MultipleAliases 验证多个 alias 的每日统计互不干扰。
func TestDailyTracking_MultipleAliases(t *testing.T) {
	us := newUsageStats()
	today := time.Now().In(beijing).Format("2006-01-02")

	us.record("alias-a", usageData{HasData: true, Prompt: 500, Completion: 300})
	us.record("alias-b", usageData{HasData: true, Prompt: 800, Completion: 400})
	us.recordError("alias-a")
	us.record("alias-a", usageData{HasData: true, Prompt: 200, Completion: 100})

	snap := us.snapshot()

	// alias-a
	a, ok := snap["alias-a"]
	if !ok {
		t.Fatal("alias-a 应有统计")
	}
	da := a.Daily[today]
	if da == nil || da.Prompt != 700 || da.Completion != 400 || da.Errors != 1 || da.Count != 2 {
		t.Fatalf("alias-a 每日统计错误: Prompt=%d Completion=%d Errors=%d Count=%d", da.Prompt, da.Completion, da.Errors, da.Count)
	}

	// alias-b
	b, ok := snap["alias-b"]
	if !ok {
		t.Fatal("alias-b 应有统计")
	}
	db := b.Daily[today]
	if db == nil || db.Prompt != 800 || db.Completion != 400 || db.Errors != 0 || db.Count != 1 {
		t.Fatalf("alias-b 每日统计错误: Prompt=%d Completion=%d Errors=%d Count=%d", db.Prompt, db.Completion, db.Errors, db.Count)
	}

	t.Log("✓ 多 alias 每日统计互不干扰")
}

// TestDailyTracking_SnapshotDeepCopy 验证 snapshot 深拷贝,修改 snapshot 不影响原数据。
func TestDailyTracking_SnapshotDeepCopy(t *testing.T) {
	us := newUsageStats()
	today := time.Now().In(beijing).Format("2006-01-02")

	us.record("test", usageData{HasData: true, Prompt: 100, Completion: 50})

	snap := us.snapshot()
	s := snap["test"]
	originalPrompt := s.Daily[today].Prompt

	// 修改 snapshot(深拷贝,不应影响原数据)
	s.Daily[today].Prompt = 999

	// 原数据不应受影响
	snap2 := us.snapshot()
	d2 := snap2["test"].Daily[today]
	if d2.Prompt != originalPrompt {
		t.Fatalf("snapshot 深拷贝失败: 修改 snapshot 后原数据从 %d 变为了 %d", originalPrompt, d2.Prompt)
	}

	t.Log("✓ snapshot 深拷贝正确")
}

// TestDailyTracking_NoData 验证没有请求时不会创建空条目。
func TestDailyTracking_NoData(t *testing.T) {
	us := newUsageStats()
	snap := us.snapshot()
	if len(snap) != 0 {
		t.Fatal("没有请求时 snapshot 应为空")
	}

	html := buildDailyHTML(snap)
	if html != "" {
		t.Fatal("没有数据时 buildDailyHTML 应返回空字符串")
	}

	t.Log("✓ 无数据时正确处理")
}

// TestDailyTracking_RecordNoUsage 验证 HasData=false 时不记录每日用量。
func TestDailyTracking_RecordNoUsage(t *testing.T) {
	us := newUsageStats()
	us.record("test", usageData{HasData: false, Prompt: 999})
	snap := us.snapshot()
	if len(snap) > 0 {
		t.Fatal("HasData=false 时不应创建统计")
	}
	t.Log("✓ HasData=false 正确忽略")
}
