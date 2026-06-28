package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ---- 单元测试: filterImageBlocks ----

func TestFilterImageBlocks_NoRules(t *testing.T) {
	// 无规则 → body 不变
	body := io.NopCloser(strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	r, l, _ := filterImageBlocks(body, nil, "api.deepseek.com")
	if l >= 0 {
		t.Errorf("无规则时应返回 -1, 得到 %d", l)
	}
	if r == nil {
		t.Fatal("返回的 reader 不应为 nil")
	}
}

func TestFilterImageBlocks_DomainMatch(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
}

func TestFilterImageBlocks_DomainNoMatch(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"claude-3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.anthropic.com")
	if l >= 0 {
		t.Fatal("域名不匹配时应返回 -1")
	}
}

func TestFilterImageBlocks_ModelMatch(t *testing.T) {
	rules := []ImageFilterRule{
		{Models: []string{"deepseek-chat"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.any.com")
	if l < 0 {
		t.Fatal("模型匹配时应返回新的 ContentLength")
	}
}

func TestFilterImageBlocks_ModelNoMatch(t *testing.T) {
	rules := []ImageFilterRule{
		{Models: []string{"deepseek-chat"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.any.com")
	if l < 0 {
		t.Fatal("模型不匹配但 body 已被读取, 应有有效 ContentLength")
	}
}

func TestFilterImageBlocks_DomainAndModelBothMatch(t *testing.T) {
	rules := []ImageFilterRule{
		{Models: []string{"deepseek-chat"}, Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("域名+模型都匹配时应返回新的 ContentLength")
	}
}

func TestFilterImageBlocks_DomainAndModelOneFails(t *testing.T) {
	rules := []ImageFilterRule{
		{Models: []string{"deepseek-chat"}, Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	// 模型匹配但域名不匹配
	body := io.NopCloser(strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.anthropic.com")
	if l >= 0 {
		t.Fatal("域名不匹配时应返回 -1")
	}
}

// ---- 单元测试: image_url → [Image] 转换 ----

func TestFilterImageBlocks_ToText(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:base64..."}}]}]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content []map[string]string
	json.Unmarshal(msg["content"], &content)

	if len(content) != 2 {
		t.Fatalf("期望 2 个 block, 得到 %d: %+v", len(content), content)
	}
	if content[0]["type"] != "text" || content[0]["text"] != "hello" {
		t.Errorf("第一个 block 应不变, 得到 %+v", content[0])
	}
	if content[1]["type"] != "text" || content[1]["text"] != "[Image]" {
		t.Errorf("image_url 应被替换为 [Image], 得到 %+v", content[1])
	}
}

func TestFilterImageBlocks_Strip(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "strip"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:base64..."}}]}]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content []map[string]string
	json.Unmarshal(msg["content"], &content)

	if len(content) != 1 {
		t.Fatalf("strip 后期望 1 个 block(图片被删), 得到 %d: %+v", len(content), content)
	}
	if content[0]["type"] != "text" || content[0]["text"] != "hello" {
		t.Errorf("非 image block 应保留, 得到 %+v", content[0])
	}
}

func TestFilterImageBlocks_AllImagesToText(t *testing.T) {
	// 所有 block 都是 image_url → 替换为 [Image] 占位符
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:1"}},{"type":"image_url","image_url":{"url":"data:2"}}]}]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content []map[string]string
	json.Unmarshal(msg["content"], &content)

	if len(content) != 2 {
		t.Fatalf("期望 2 个 [Image] 占位符, 得到 %d: %+v", len(content), content)
	}
	for i, block := range content {
		if block["type"] != "text" || block["text"] != "[Image]" {
			t.Errorf("block[%d] 应是 [Image], 得到 %+v", i, block)
		}
	}
}

func TestFilterImageBlocks_AllImagesStrip(t *testing.T) {
	// 所有 block 都是 image_url, strip → 插入 [Image] 防止空数组
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "strip"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:1"}}]}]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content []map[string]string
	json.Unmarshal(msg["content"], &content)

	if len(content) != 1 {
		t.Fatalf("全部 strip 后应插入 1 个 [Image] 占位符, 得到 %d: %+v", len(content), content)
	}
	if content[0]["type"] != "text" || content[0]["text"] != "[Image]" {
		t.Errorf("应是 [Image] 占位符, 得到 %+v", content[0])
	}
}

func TestFilterImageBlocks_StringContent(t *testing.T) {
	// content 是字符串（非数组）→ 不变
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content string
	json.Unmarshal(msg["content"], &content)

	if content != "hello" {
		t.Errorf("字符串 content 不应被修改, 得到 %q", content)
	}
}

func TestFilterImageBlocks_MalformedBody(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	// 无效 JSON → 回退原始 body
	body := io.NopCloser(strings.NewReader(`this is not json`))
	_, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("即使 body 无效也应有有效 ContentLength")
	}
}

func TestFilterImageBlocks_NoMessages(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	// 没有 messages 字段 → 不变
	input := `{"model":"deepseek-chat","stream":true}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	if string(data) != input {
		t.Errorf("无 messages 时 body 不应变化: got %s", string(data))
	}
}

func TestFilterImageBlocks_WildcardModel(t *testing.T) {
	rules := []ImageFilterRule{
		{Models: []string{"*"}, Action: "to_text"},
	}
	body := io.NopCloser(strings.NewReader(`{"model":"any-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`))
	_, l, _ := filterImageBlocks(body, rules, "api.any.com")
	if l < 0 {
		t.Fatal("通配符 * 应匹配所有模型")
	}
}

func TestFilterImageBlocks_MultipleMessages(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[
		{"role":"user","content":"just text"},
		{"role":"user","content":[{"type":"text","text":"desc"},{"type":"image_url","image_url":{"url":"data:..."}}]}
	]}`
	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	var resp map[string]json.RawMessage
	json.Unmarshal(data, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)

	// 第一条消息:字符串 content,应不变
	var msg0 map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg0)
	var text string
	json.Unmarshal(msg0["content"], &text)
	if text != "just text" {
		t.Errorf("第1条字符串 content 不应变, 得到 %q", text)
	}

	// 第二条消息:数组 content,image_url 应被替换
	var msg1 map[string]json.RawMessage
	json.Unmarshal(msgs[1], &msg1)
	var content []map[string]string
	json.Unmarshal(msg1["content"], &content)
	if len(content) != 2 {
		t.Fatalf("第2条应有 2 个 block, 得到 %d: %+v", len(content), content)
	}
	if content[1]["type"] != "text" || content[1]["text"] != "[Image]" {
		t.Errorf("image_url 应被替换为 [Image], 得到 %+v", content[1])
	}
}

// ---- 集成测试: 通过代理转发验证 ----

func TestProxyFilterImageBlocks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证上游收到的 body 已被过滤（image_url 应被替换为 [Image]）
		data, _ := io.ReadAll(r.Body)
		var req map[string]json.RawMessage
		json.Unmarshal(data, &req)
		var msgs []json.RawMessage
		json.Unmarshal(req["messages"], &msgs)
		var msg map[string]json.RawMessage
		json.Unmarshal(msgs[0], &msg)
		var content []map[string]string
		json.Unmarshal(msg["content"], &content)

		for i, block := range content {
			if block["type"] == "image_url" {
				t.Errorf("block[%d] 仍然是 image_url, 应被过滤", i)
			}
		}
		// 验证至少有一个占位符
		foundPlaceholder := false
		for _, block := range content {
			if block["type"] == "text" && block["text"] == "[Image]" {
				foundPlaceholder = true
				break
			}
		}
		if !foundPlaceholder {
			t.Errorf("应存在 [Image] 占位符: content=%+v", content)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","model":"deepseek-chat","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer backend.Close()

	rules := []ImageFilterRule{
		{Domains: []string{backend.Listener.Addr().String()}, Action: "to_text"},
	}

	proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", rules, nil))
	defer proxy.Close()

	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`
	resp, err := http.Post(proxy.URL+"/"+backend.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestProxyPassthroughNoFilter(t *testing.T) {
	// 透传模式(无规则) → body 不变
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(data), "image_url") {
			t.Error("无规则时应透传原始 body(image_url 应保留)")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","model":"gpt-4","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer backend.Close()

	proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", nil, nil))
	defer proxy.Close()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`
	resp, err := http.Post(proxy.URL+"/"+backend.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// ---- 集成测试: 模拟 DeepSeek 场景 ----
//
// 模拟后端：收到 image_url → 返回 400（模拟 DeepSeek 报错），否则返回 200。
// 由此验证：
//   1. 无 filter → 带 image_url 的请求报错
//   2. 有 filter → 同一请求被过滤后正常通过

// deepSeekMock 模拟 DeepSeek 后端：body 含 image_url 则拒绝，否则成功。
func deepSeekMock(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	if strings.Contains(string(data), `"type":"image_url"`) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Failed to deserialize the JSON body: unknown variant 'image_url', expected 'text'","code":"invalid_request_error"}}`))
		return
	}
	// 成功响应
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"id":"1","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
}

func TestFilterImageBeforeAfter(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(deepSeekMock))
	defer backend.Close()

	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:base64,..."}}]}]}`

	// ---- 测试 1: 无 filter → 应该 400 报错 ----
	t.Run("无filter时image_url请求应报错", func(t *testing.T) {
		proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", nil, nil))
		defer proxy.Close()

		resp, err := http.Post(proxy.URL+"/"+backend.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("期望 400, 得到 %d", resp.StatusCode)
		}
		data, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(data), "unknown variant") {
			t.Errorf("响应应包含 'unknown variant' 错误信息, 得到: %s", string(data))
		}
	})

	// ---- 测试 2: 有 filter → 应该 200 成功 ----
	t.Run("有filter时image_url被过滤应正常通过", func(t *testing.T) {
		rules := []ImageFilterRule{
			{Domains: []string{backend.Listener.Addr().String()}, Action: "to_text"},
		}
		proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", rules, nil))
		defer proxy.Close()

		resp, err := http.Post(proxy.URL+"/"+backend.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("期望 200, 得到 %d", resp.StatusCode)
		}
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		choices, _ := result["choices"].([]interface{})
		if len(choices) == 0 {
			t.Fatal("响应应包含 choices")
		}
	})
}

// ---- 单元测试: domainMatchesAny / modelMatchesAny ----

func TestDomainMatchesAny(t *testing.T) {
	tests := []struct {
		domain string
		list   []string
		want   bool
	}{
		{"api.deepseek.com", []string{"deepseek"}, true},
		{"api.deepseek.com", []string{"api.deepseek.com"}, true},
		{"api.deepseek.com", []string{"deepseek.com"}, true},
		{"api.deepseek.com", []string{"openai.com"}, false},
		{"api.deepseek.com", []string{}, false},
		{"api.DEEPSEEK.com", []string{"DeepSeek"}, true}, // 大小写不敏感
	}
	for _, tt := range tests {
		got := domainMatchesAny(tt.domain, tt.list)
		if got != tt.want {
			t.Errorf("domainMatchesAny(%q, %v) = %v, want %v", tt.domain, tt.list, got, tt.want)
		}
	}
}

func TestModelMatchesAny(t *testing.T) {
	tests := []struct {
		model string
		list  []string
		want  bool
	}{
		{"deepseek-chat", []string{"deepseek-chat"}, true},
		{"deepseek-chat", []string{"deepseek"}, true},
		{"deepseek-chat", []string{"gpt-4"}, false},
		{"deepseek-chat", []string{"*"}, true},
		{"DeepSeek-Chat", []string{"deepseek-chat"}, true}, // 大小写不敏感
		{"gpt-4-vision", []string{"gpt-4"}, true},          // 前缀匹配
	}
	for _, tt := range tests {
		got := modelMatchesAny(tt.model, tt.list)
		if got != tt.want {
			t.Errorf("modelMatchesAny(%q, %v) = %v, want %v", tt.model, tt.list, got, tt.want)
		}
	}
}

// ---- 单元测试: needBodyScan ----

func TestNeedBodyScan(t *testing.T) {
	tests := []struct {
		name   string
		rules  []ImageFilterRule
		domain string
		want   bool
	}{
		{
			name:   "域名匹配",
			rules:  []ImageFilterRule{{Domains: []string{"deepseek.com"}}},
			domain: "api.deepseek.com",
			want:   true,
		},
		{
			name:   "域名不匹配",
			rules:  []ImageFilterRule{{Domains: []string{"openai.com"}}},
			domain: "api.deepseek.com",
			want:   false,
		},
		{
			name:   "只有模型条件",
			rules:  []ImageFilterRule{{Models: []string{"deepseek-chat"}}},
			domain: "any.domain.com",
			want:   true,
		},
		{
			name:   "空规则",
			rules:  []ImageFilterRule{},
			domain: "any.domain.com",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needBodyScan(tt.rules, tt.domain)
			if got != tt.want {
				t.Errorf("needBodyScan(%v, %q) = %v, want %v", tt.rules, tt.domain, got, tt.want)
			}
		})
	}
}

// ---- 直接测试 filterImageBlocksInData ----

func TestFilterImageBlocksInData_Direct(t *testing.T) {
	// 使用与集成测试完全相同的 body
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`
	result, err := filterImageBlocksInData([]byte(input), "to_text")
	if err != nil {
		t.Fatalf("filterImageBlocksInData 失败: %v", err)
	}
	var resp map[string]json.RawMessage
	json.Unmarshal(result, &resp)
	var msgs []json.RawMessage
	json.Unmarshal(resp["messages"], &msgs)
	var msg map[string]json.RawMessage
	json.Unmarshal(msgs[0], &msg)
	var content []map[string]string
	json.Unmarshal(msg["content"], &content)
	if len(content) != 2 {
		t.Fatalf("期望 2 个 block, 得到 %d", len(content))
	}
	if content[1]["text"] != "[Image]" {
		t.Errorf("image_url 未被替换, 得到 %+v", content[1])
	}
}

// ---- YAML 配置加载测试 ----

func TestLoadImageFilterFromYAML(t *testing.T) {
	// 创建一个带 image_filter 的临时文件
	yaml := `
glm:
  key: sk-glm-test
deepseek:
  key: sk-deepseek-test
image_filter:
  - models: ["deepseek-chat", "deepseek-coder"]
    action: to_text
  - domains: ["api.deepseek.com"]
    action: strip
  - models: ["claude-3"]
    domains: ["api.anthropic.com"]
    action: to_text
`
	ks := newKeyStore()
	tmpDir := t.TempDir()
	path := tmpDir + "/keys.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}

	rules := ks.getImageFilter()
	if len(rules) != 3 {
		t.Fatalf("期望 3 条规则, 得到 %d", len(rules))
	}

	// 规则1: 模型匹配
	if len(rules[0].Models) != 2 || rules[0].Models[0] != "deepseek-chat" {
		t.Errorf("规则1 models 错误: %v", rules[0].Models)
	}
	if rules[0].Action != "to_text" {
		t.Errorf("规则1 action = %q, 期望 to_text", rules[0].Action)
	}

	// 规则2: 域名匹配
	if len(rules[1].Domains) != 1 || rules[1].Domains[0] != "api.deepseek.com" {
		t.Errorf("规则2 domains 错误: %v", rules[1].Domains)
	}
	if rules[1].Action != "strip" {
		t.Errorf("规则2 action = %q, 期望 strip", rules[1].Action)
	}

	// 规则3: 两者都匹配
	if len(rules[2].Models) != 1 || len(rules[2].Domains) != 1 {
		t.Errorf("规则3 应该同时有 models 和 domains")
	}

	// 验证 alias 配置正常加载
	if _, ok := ks.lookup("glm"); !ok {
		t.Error("glm alias 应存在")
	}
	if _, ok := ks.lookup("deepseek"); !ok {
		t.Error("deepseek alias 应存在")
	}
}

func TestLoadYAMLWithoutImageFilter(t *testing.T) {
	// 没有 image_filter 时, 向后兼容
	yaml := `
glm:
  key: sk-glm-test
`
	ks := newKeyStore()
	tmpDir := t.TempDir()
	path := tmpDir + "/keys.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}

	rules := ks.getImageFilter()
	if len(rules) != 0 {
		t.Errorf("无 image_filter 时应为空, 得到 %d 条规则", len(rules))
	}

	cfg, ok := ks.lookup("glm")
	if !ok || cfg.Key != "sk-glm-test" {
		t.Error("alias 配置应正常加载")
	}
}

func TestLoadEmptyKeysYAML(t *testing.T) {
	// 空文件
	ks := newKeyStore()
	tmpDir := t.TempDir()
	path := tmpDir + "/empty.yaml"
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ks.load(path); err != nil {
		t.Fatal(err)
	}
}

// ---- Content-Length 正确性验证 ----

func TestFilterImageBlocksContentLength(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`

	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	if int(l) != len(data) {
		t.Errorf("ContentLength %d 与实际数据长度 %d 不一致", l, len(data))
	}
}

func TestFilterImageBlocksUnchangedContentLength(t *testing.T) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`

	body := io.NopCloser(strings.NewReader(input))
	r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
	if l < 0 {
		t.Fatal("规则命中时应返回新的 ContentLength")
	}
	data, _ := io.ReadAll(r)
	r.Close()

	if int(l) != len(data) {
		t.Errorf("ContentLength %d 与实际数据长度 %d 不一致", l, len(data))
	}
	if string(data) != input {
		t.Errorf("无 image_url 时 body 不应变化")
	}
}

// ---- 性能: body 较大时 ----

func BenchmarkFilterImageBlocks(b *testing.B) {
	rules := []ImageFilterRule{
		{Domains: []string{"api.deepseek.com"}, Action: "to_text"},
	}
	// 构造一个约 100KB 的 body
	var msgParts []string
	for i := 0; i < 100; i++ {
		msgParts = append(msgParts, `{"type":"text","text":"hello world hello world hello world"}`)
	}
	msgParts = append(msgParts, `{"type":"image_url","image_url":{"url":"data:base64...base64..."}}`)
	content := strings.Join(msgParts, ",")

	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[` + content + `]}]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := io.NopCloser(strings.NewReader(input))
		r, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
		if l < 0 {
			b.Fatal("期望规则命中")
		}
		io.ReadAll(r)
		r.Close()
	}
}

func BenchmarkFilterImageBlocksNoMatch(b *testing.B) {
	// 规则不匹配时的开销
	rules := []ImageFilterRule{
		{Domains: []string{"api.other.com"}, Action: "to_text"},
	}
	input := `{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := io.NopCloser(strings.NewReader(input))
		_, l, _ := filterImageBlocks(body, rules, "api.deepseek.com")
		if l >= 0 {
			b.Fatal("期望不命中")
		}
	}
}
