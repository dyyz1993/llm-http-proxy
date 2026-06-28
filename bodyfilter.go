// bodyfilter 提供请求 body 的 image_url 内容块过滤/转换功能。
//
// 配置驱动：在 keys.yaml 中添加 image_filter 规则段，支持按模型名和/或目标域名匹配。
// 匹配命中后将 messages 中 type=image_url 的内容块替换为 [Image] 文本占位符（to_text）
// 或直接删除（strip）。
//
// 设计原则：
//   - 零开销：无规则时不做任何 body 读取/解析
//   - 快速跳过：仅域名条件的规则可跳过 body 解析直接匹配
//   - 容错：JSON 解析失败静默回退原始 body，不破坏请求
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"strings"
)

// ImageFilterRule 定义一条 image_url 过滤规则。
// Models 和 Domains 至少一个不为空，两者都设置时为 AND 条件。
type ImageFilterRule struct {
	Models  []string `yaml:"models"`  // 匹配的模型名（子串匹配，大小写不敏感）。["*"] 匹配全部
	Domains []string `yaml:"domains"` // 匹配的目标域名（子串匹配，大小写不敏感）
	Action  string   `yaml:"action"`  // "to_text":替换为 [Image]；"strip":直接删除
}

// filterImageBlocks 检查 image_filter 规则，命中则过滤 body 中的 image_url 内容块。
// 参数:
//   - body: 原始请求 body（会被读取并关闭）
//   - rules: image_filter 规则列表
//   - targetDomain: 上游目标域名（如 "api.deepseek.com"）
//
// 返回值:
//   - 新的 body reader（原始 body 被读完后关闭）
//   - -1 表示无规则、未命中或解析失败，调用方应使用原始 ContentLength
//   - true 表示 body 被实际修改（image_url 被替换/删除）
func filterImageBlocks(body io.ReadCloser, rules []ImageFilterRule, targetDomain string) (io.ReadCloser, int64, bool) {
	if len(rules) == 0 {
		return body, -1, false
	}

	// 快速检查：是否有规则可能命中当前请求
	if !needBodyScan(rules, targetDomain) {
		return body, -1, false
	}

	// 读取完整 body
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		log.Printf("filterImageBlocks: 读取 body 失败: %v", err)
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), false
	}

	// 提取 model 字段
	reqModel := extractModelFromBody(data)

	// 检查是否有规则最终命中
	matched := matchRules(rules, reqModel, targetDomain)
	if len(matched) == 0 {
		// 未命中，返回原始 body
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), false
	}

	// 取第一个命中规则的 action
	action := matched[0].Action

	// 执行过滤
	newData, err := filterImageBlocksInData(data, action)
	if err != nil {
		log.Printf("filterImageBlocks: 过滤失败(%d bytes): %v, 回退原始 body", len(data), err)
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), false
	}

	return io.NopCloser(bytes.NewReader(newData)), int64(len(newData)), true
}

// needBodyScan 快速判断是否需要读取 body 做进一步检查。
// 返回 true 时调用方应当读取 body。
func needBodyScan(rules []ImageFilterRule, targetDomain string) bool {
	for _, rule := range rules {
		if len(rule.Domains) == 0 {
			// 无域名条件 → 需要 body 检查 model
			return true
		}
		if domainMatchesAny(targetDomain, rule.Domains) {
			return true
		}
	}
	return false
}

// extractModelFromBody 从 JSON body 中快速提取 model 字段。
// 返回空字符串表示无法提取。
func extractModelFromBody(data []byte) string {
	var raw struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	return strings.TrimSpace(raw.Model)
}

// domainMatchesAny 检查 domain 是否匹配 domains 列表中任意一个。
// 子串匹配，大小写不敏感。
func domainMatchesAny(domain string, domains []string) bool {
	domain = strings.ToLower(domain)
	for _, d := range domains {
		if strings.Contains(domain, strings.ToLower(d)) {
			return true
		}
	}
	return false
}

// modelMatchesAny 检查 model 是否匹配 models 列表中任意一个。
// 子串匹配，大小写不敏感。["*"] 匹配全部。
func modelMatchesAny(model string, models []string) bool {
	model = strings.ToLower(model)
	for _, m := range models {
		if m == "*" {
			return true
		}
		if strings.Contains(model, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// matchRules 返回所有命中的规则。没有命中时返回 nil。
func matchRules(rules []ImageFilterRule, model, domain string) []ImageFilterRule {
	var matched []ImageFilterRule
	for _, rule := range rules {
		// 域名检查：rule.Domains 为空则自动通过
		if len(rule.Domains) > 0 && !domainMatchesAny(domain, rule.Domains) {
			continue
		}
		// 模型检查：rule.Models 为空则自动通过
		if len(rule.Models) > 0 && !modelMatchesAny(model, rule.Models) {
			continue
		}
		matched = append(matched, rule)
	}
	return matched
}

// filterImageBlocksInData 执行实际的 body 修改。
// 遍历 messages 中的 content 数组，过滤掉 type=image_url 的 block。
func filterImageBlocksInData(data []byte, action string) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}

	messagesRaw, ok := req["messages"]
	if !ok {
		return data, nil // 没有 messages 字段，无需处理
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, err
	}

	modified := false
	for i, msgRaw := range messages {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}

		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}

		// 尝试作为 content 数组解析（[{"type":"text",...}, {"type":"image_url",...}]）
		var contentBlocks []json.RawMessage
		if err := json.Unmarshal(contentRaw, &contentBlocks); err != nil {
			continue // 字符串 content（如 "Hello"），跳过
		}

		newBlocks, changed := filterBlocks(contentBlocks, action)
		if changed {
			modified = true
			newContent, err := json.Marshal(newBlocks)
			if err != nil {
				return nil, err
			}
			msg["content"] = newContent
			newMsg, err := json.Marshal(msg)
			if err != nil {
				return nil, err
			}
			messages[i] = newMsg
		}
	}

	if !modified {
		return data, nil
	}

	newMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	req["messages"] = newMessages

	return json.Marshal(req)
}

// filterBlocks 处理单个 message 的 content 数组，根据 action 过滤 image_url 块。
// 返回新数组和是否有改动。
func filterBlocks(blocks []json.RawMessage, action string) ([]json.RawMessage, bool) {
	var result []json.RawMessage
	changed := false

	for _, block := range blocks {
		var blockMap struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &blockMap); err != nil {
			// 无法解析 → 保留
			result = append(result, block)
			continue
		}

		if blockMap.Type != "image_url" {
			// 非 image_url → 保留
			result = append(result, block)
			continue
		}

		// 这是 image_url block
		changed = true
		switch action {
		case "strip":
			// 直接删除，不追加
		default: // "to_text"
			placeholder, _ := json.Marshal(map[string]string{"type": "text", "text": "[Image]"})
			result = append(result, placeholder)
		}
	}

	// 如果全部被删光，插入一个占位符防止空数组
	if len(result) == 0 {
		placeholder, _ := json.Marshal(map[string]string{"type": "text", "text": "[Image]"})
		result = append(result, placeholder)
		changed = true
	}

	return result, changed
}
