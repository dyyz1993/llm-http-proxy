// stream_watch.go — SSE 流健康守卫
//
// 背景(2026-11-08 排查): z.ai 上游有两种高频故障形态——
//   B. 开场死(60%): 发了 message_start+ping 后 worker 挂掉,再也不吐内容事件
//   A/C. 中途断: 内容流到一半或差收尾时连接被掐
// 本文件提供:
//   sseWatcher  — SSE 事件流最小分类器(anthropic 风格 event: 行 +
//                 data-only 风格载荷 type 字段,两种 SSE 都兼容)
//   streamGuard — 开场死判定:收到响应体后 stall 时长内没见到内容事件 →
//                 取消上游请求(客户端无感知,group 换人重试)
//   sseTerminalEvents — anthropic 流结束时内容完整但缺 message_stop → 合成收尾;
//                 内容不完整 → 注入 error 事件让客户端快速失败重试。
//                 data-only 流(OpenAI 风格)有自己的结束标记,不做合成。

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// streamFirstContentStall 开场死判定时长:收到响应体首字节后,这么久没见到
// 内容事件(data 载荷非生命周期事件)即判死。正常流 message_start → 首个内容
// 事件间隔在秒级;观测到的开场死流永远等不到。
// 包级变量以便测试覆盖;生产值 15s(正常 TTFB→首内容间隔为秒级,15s 留足余量)。
var streamFirstContentStall = 15 * time.Second

// sameMemberRetryMaxDelay 同成员重试的快速失败上限:失败耗时超过该值不再重试
// 同一成员(观测到 z.ai 长挂 3m46s 后 502,原逻辑会再挂同一成员 1m1s,客户
// 端干等近 5 分钟)。瞬时错误(秒级)才值得原地重试。包级变量便于测试覆盖。
var sameMemberRetryMaxDelay = 10 * time.Second

// shouldRetrySameMember 判断是否对同一成员原地重试一次。
// 429(额度满)/401(key 失效)是确定性错误,重试无意义;慢失败(长挂)直接换人。
func shouldRetrySameMember(status, tryNum int, attemptDur time.Duration) bool {
	return tryNum == 0 && status != 429 && status != 401 && attemptDur < sameMemberRetryMaxDelay
}

// ---------- sseWatcher: SSE 事件流最小分类器 ----------

// sseWatcher 逐块喂入 SSE 字节,扫描 "event:" 行与 data 载荷的 type 字段维护流状态。
// 兼容两种 SSE 风格:
//   - anthropic 风格(event: + data: 成对,api.z.ai/api/anthropic/v1/messages)
//   - data-only 风格(只有 data: 行,如 open.bigmodel.cn /v4/chat/completions、OpenAI)
//
// data 行不完整解析(可能很大),只看前 256 字节判 type;残行超长时安全丢弃
// (丢的只会是 data 正文,不丢 event: 行)。
type sseWatcher struct {
	partial         []byte // 未完结的行
	sawAnthropic    bool   // 见过 event: 行(anthropic 风格流)
	sawContent      bool   // 见过内容事件/数据载荷(生命周期事件不算)
	openBlocks      int    // 已开未闭的 content_block 数
	sawBlockStop    bool   // 见过至少一个 content_block_stop
	sawMessageDelta bool   // 见过 message_delta(收尾 usage)
	sawMessageStop  bool   // 见过 message_stop(流正常结束)
	sawDone         bool   // 见过 data-only 流的 [DONE] 结束标记
	sawError        bool   // 见过 error 事件(上游显式报错)
}

// sseKeepaliveTypes data 载荷中的生命周期事件类型(不算内容)。
var sseKeepaliveTypes = map[string]bool{"message_start": true, "ping": true, "pong": true}

// feed 喂入一个 chunk。
func (w *sseWatcher) feed(p []byte) {
	w.partial = append(w.partial, p...)
	for {
		idx := bytes.IndexByte(w.partial, '\n')
		if idx < 0 {
			break
		}
		line := string(w.partial[:idx])
		w.partial = w.partial[idx+1:]
		w.classifyLine(line)
	}
	// 残行保护:超长残行只可能是 data 正文行(event: 行都很短),丢弃防内存膨胀
	if len(w.partial) > 8192 {
		w.partial = nil
	}
}

func (w *sseWatcher) classifyLine(line string) {
	if strings.HasPrefix(line, "event:") {
		w.sawAnthropic = true
		switch strings.TrimSpace(strings.TrimPrefix(line, "event:")) {
		case "content_block_start":
			w.sawContent = true
			w.openBlocks++
		case "content_block_delta":
			w.sawContent = true
		case "content_block_stop":
			w.sawBlockStop = true
			if w.openBlocks > 0 {
				w.openBlocks--
			}
		case "message_delta":
			w.sawMessageDelta = true
		case "message_stop":
			w.sawMessageStop = true
		case "error":
			w.sawError = true
		}
		return
	}
	if strings.HasPrefix(line, "data:") {
		w.classifyData(strings.TrimPrefix(line, "data:"))
	}
}

// classifyData 判定 data 载荷是否为内容(区别于 message_start/ping 等生命周期事件)。
// z.ai 的 anthropic 端点每个 data 都带 "type";data-only 风格的 chunk 无 type 字段。
func (w *sseWatcher) classifyData(payload string) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		w.sawDone = true
		return
	}
	head := payload
	if len(head) > 256 {
		head = head[:256]
	}
	typ := extractJSONType(head)
	if typ != "" {
		if sseKeepaliveTypes[typ] {
			return // 生命周期事件,不算内容
		}
		w.sawContent = true
		switch typ {
		case "content_block_start":
			w.openBlocks++
		case "content_block_stop":
			w.sawBlockStop = true
			if w.openBlocks > 0 {
				w.openBlocks--
			}
		case "message_stop":
			w.sawMessageStop = true
		case "message_delta":
			w.sawMessageDelta = true
		}
		return
	}
	// 无 type 字段(data-only 风格 chunk)→ 内容
	w.sawContent = true
}

// extractJSONType 从 JSON 头部提取 "type" 字段值(截断的 JSON 也尽力提取)。
func extractJSONType(head string) string {
	i := strings.Index(head, `"type"`)
	if i < 0 {
		return ""
	}
	rest := head[i+len(`"type"`):]
	rest = strings.TrimLeft(rest, " \t:")
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	// 值被截断(无闭引号)时取剩余部分:截断的值匹配不到任何已知类型,
	// 与空串的分类效果一致(都按内容处理),但保留部分信息便于排查
	k := strings.IndexByte(rest[j+1:], '"')
	if k < 0 {
		return rest[j+1:]
	}
	return rest[j+1 : j+1+k]
}

// hasContent 是否见过内容事件/数据(ping/message_start 不算)。
func (w *sseWatcher) hasContent() bool { return w.sawContent }

// contentComplete 内容是否完整:anthropic 风格流中内容块全部闭合。
// data-only 流没有块概念,永远 false → 不做收尾合成(它们有自己的结束标记)。
func (w *sseWatcher) contentComplete() bool {
	return w.sawAnthropic && w.sawContent && w.sawBlockStop && w.openBlocks == 0
}

// finished 流是否已终结(message_stop / error 事件 / data-only 的 [DONE])。
func (w *sseWatcher) finished() bool { return w.sawMessageStop || w.sawError || w.sawDone }

// ---------- streamGuard: 开场死判定 ----------

// streamGuard 伴随一次 group 成员转发尝试:
//   - SSE 流式响应时,在收到响应体首字节后启动 stall 定时器;
//   - 期间任何内容事件到达 → 放行(通知 holder 释放 hold 缓存),定时器作废;
//   - stall 到点仍无内容 → 置 aborted 并取消上游请求 → group 循环换人。
type streamGuard struct {
	mu          sync.Mutex
	watcher     sseWatcher
	stall       time.Duration
	armed       bool // 定时器已武装(isStream 时首字节触发)
	released    bool // 已见到内容事件并放行
	aborted     bool // 开场死,已取消上游
	firstByteAt time.Time
	timer       *time.Timer
	cancel      context.CancelFunc         // 取消上游请求(不影响客户端请求)
	holder      interface{ releaseHold() } // groupWriter(持 hold 缓存的一方)
}

func newStreamGuard(stall time.Duration) *streamGuard {
	return &streamGuard{stall: stall}
}

// arm 注入上游取消函数(在构造上游请求上下文后调用)。
func (sg *streamGuard) arm(cancel context.CancelFunc) {
	sg.mu.Lock()
	sg.cancel = cancel
	sg.mu.Unlock()
}

// feed 喂入响应体字节;见到内容事件时放行,首个字节时武装 stall 定时器。
// 仅 SSE 流式响应调用(非 SSE 不喂,不武装,零行为变化)。
func (sg *streamGuard) feed(p []byte) {
	sg.mu.Lock()
	if sg.aborted {
		sg.mu.Unlock()
		return
	}
	if sg.firstByteAt.IsZero() && sg.stall > 0 {
		sg.firstByteAt = time.Now()
		sg.armed = true
		sg.timer = time.AfterFunc(sg.stall, sg.onStall)
	}
	sg.watcher.feed(p)
	meaningful := sg.watcher.hasContent() || sg.watcher.finished()
	sg.mu.Unlock()

	if meaningful {
		sg.markReleased()
	}
}

// onStall stall 到点仍无内容事件 → 开场死,取消上游请求。
func (sg *streamGuard) onStall() {
	sg.mu.Lock()
	if sg.released || sg.aborted {
		sg.mu.Unlock()
		return
	}
	sg.aborted = true
	cancel := sg.cancel
	sg.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// markReleased 见到内容事件:停定时器,通知 holder 放行 hold 缓存。
func (sg *streamGuard) markReleased() {
	sg.mu.Lock()
	if sg.released || sg.aborted {
		sg.mu.Unlock()
		return
	}
	sg.released = true
	if sg.timer != nil {
		sg.timer.Stop()
	}
	holder := sg.holder
	sg.mu.Unlock()
	if holder != nil {
		holder.releaseHold()
	}
}

// abortedNow 开场死是否已判定(转发结束后由 group 循环检查)。
func (sg *streamGuard) abortedNow() bool {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	return sg.aborted
}

// terminalEvents 流结束后判定需要合成的终止事件(见 sseTerminalEvents)。
func (sg *streamGuard) terminalEvents(streamedBytes int) []byte {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	return sseTerminalEvents(&sg.watcher, streamedBytes)
}

// ---------- 终止事件合成 ----------

// sseTerminalEvents 上游流结束后,按流状态决定是否合成终止事件:
//   - 已见 message_stop/error/[DONE] → 不需要(流自己走完了)
//   - anthropic 流内容完整(块全闭合)但缺收尾 → 合成 message_delta(估算 usage)
//   - message_stop → 客户端正常收尾,已到手的答案不浪费
//   - 有内容但块未闭合(中途断) → 注入 error 事件 → 客户端立即明确失败并重试,
//     而不是傻等 EOF
//   - data-only 流(无 event: 行) → 不做任何合成(它们有自己的结束标记,
//     截断与否我们无法可靠判定,保持透传)
//   - 没有任何内容(开场死/空流) → 不合成(group 层已换人处理)
func sseTerminalEvents(w *sseWatcher, streamedBytes int) []byte {
	// data-only 流不做合成:它们有自己的结束标记([DONE]),且没有块状态,
	// 截断与否无法可靠判定——一律保持透传。
	if !w.sawAnthropic || w.finished() {
		return nil
	}
	if w.contentComplete() {
		est := streamedBytes / 5 // 粗估:SSE 帧含 event:/data:/JSON 开销,净 token 更少
		if est < 1 {
			est = 1
		}
		return []byte(fmt.Sprintf("event: message_delta\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", est))
	}
	if w.hasContent() {
		return []byte("event: error\n" +
			"data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream ended before completion\"}}\n\n")
	}
	return nil
}
