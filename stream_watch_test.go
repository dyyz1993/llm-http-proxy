package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 流 fixture ----------

// anthropic 全生命周期流(正常完成)
const anthropicFullStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n" +
	"event: ping\n" +
	`data: {"type":"ping"}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"你好"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"世界"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// anthropic 开场死流:message_start+ping 之后什么都没有
const anthropicStillbornStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_2"}}` + "\n\n" +
	"event: ping\n" +
	`data: {"type":"ping"}` + "\n\n"

// anthropic 中途断流:内容块开着就断了(没有 content_block_stop/message_stop)
const anthropicMidCutStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_3"}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"写到一半"}}` + "\n\n"

// anthropic 差收尾流:内容块全部闭合但没有 message_delta/message_stop
const anthropicNoTrailerStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_4"}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"完整答案"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n"

// data-only 流(OpenAI 风格,正常完成)
const dataOnlyCompleteStream = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
	"data: [DONE]\n\n"

// data-only 纯心跳形态:全部是 ping 载荷
const dataOnlyKeepaliveStream = "data: {\"type\":\"ping\"}\n\n" +
	"data: {\"type\":\"ping\"}\n\n"

// ---------- sseWatcher 分类 ----------

func TestSSEWatcher_AnthropicFullStream(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(anthropicFullStream))
	if !w.hasContent() {
		t.Error("完整流应有内容")
	}
	if !w.contentComplete() {
		t.Error("完整流应判定内容完整")
	}
	if !w.finished() {
		t.Error("完整流应已终结(message_stop)")
	}
	if ev := sseTerminalEvents(&w, 1000); ev != nil {
		t.Errorf("完整流不应合成终止事件,得到: %s", ev)
	}
}

func TestSSEWatcher_Stillborn(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(anthropicStillbornStream))
	if w.hasContent() {
		t.Error("开场死流不应判定有内容")
	}
	if w.contentComplete() {
		t.Error("开场死流不应判定内容完整")
	}
	if ev := sseTerminalEvents(&w, 640); ev != nil {
		t.Errorf("开场死流不应合成终止事件(换人由 group 层处理),得到: %s", ev)
	}
}

func TestSSEWatcher_MidCut(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(anthropicMidCutStream))
	if !w.hasContent() {
		t.Error("中途断流应有内容")
	}
	if w.contentComplete() {
		t.Error("中途断流不应判定内容完整(块未闭合)")
	}
	ev := sseTerminalEvents(&w, 500)
	if ev == nil || !strings.Contains(string(ev), `"type":"error"`) {
		t.Errorf("中途断流应注入 error 事件,得到: %q", ev)
	}
}

func TestSSEWatcher_NoTrailer(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(anthropicNoTrailerStream))
	if !w.contentComplete() {
		t.Error("差收尾流应判定内容完整(块全闭合)")
	}
	if w.finished() {
		t.Error("差收尾流不应判定已终结")
	}
	ev := sseTerminalEvents(&w, 1000)
	if ev == nil || !strings.Contains(string(ev), "message_stop") {
		t.Errorf("差收尾流应合成 message_stop 收尾,得到: %q", ev)
	}
	if !strings.Contains(string(ev), "message_delta") {
		t.Errorf("合成收尾应含 message_delta(带估算 usage),得到: %q", ev)
	}
}

func TestSSEWatcher_DataOnly(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(dataOnlyCompleteStream))
	if !w.hasContent() {
		t.Error("data-only 流应判定有内容")
	}
	if !w.finished() {
		t.Error("data-only 流见 [DONE] 应判定终结")
	}
	if ev := sseTerminalEvents(&w, 1000); ev != nil {
		t.Errorf("data-only 流不应合成任何终止事件,得到: %s", ev)
	}
}

func TestSSEWatcher_DataOnlyKeepalive(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(dataOnlyKeepaliveStream))
	if w.hasContent() {
		t.Error("纯 ping 载荷不应判定有内容")
	}
}

// 逐字节喂入,验证跨 chunk 的行拼接
func TestSSEWatcher_ByteByByte(t *testing.T) {
	var w sseWatcher
	for _, b := range []byte(anthropicFullStream) {
		w.feed([]byte{b})
	}
	if !w.hasContent() || !w.contentComplete() || !w.finished() {
		t.Error("逐字节喂入的分类结果应与整块喂入一致")
	}
}

// 超长 data 行触发挥残行保护后,后续 event 行仍能正常识别
func TestSSEWatcher_LongDataLine(t *testing.T) {
	var w sseWatcher
	w.feed([]byte(`data: {"type":"content_block_delta","thinking":"` + strings.Repeat("x", 20000) + `"}}` + "\n"))
	w.feed([]byte("event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"))
	if !w.hasContent() {
		t.Error("超长 data 行后的内容事件应被正常识别")
	}
	if !w.sawBlockStop {
		t.Error("超长残行被丢弃后,后续 content_block_stop 应正常识别")
	}
}

func TestExtractJSONType(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"type":"ping"}`, "ping"},
		{`{"type": "content_block_delta","index":0}`, "content_block_delta"},
		{`{"id":"c1","choices":[]}`, ""},
		{`{"typo":"x"}`, ""},
		{`{"type":"content_bl`, "content_bl"}, // 截断 JSON 尽力提取
		{``, ""},
	}
	for _, c := range cases {
		if got := extractJSONType(c.in); got != c.want {
			t.Errorf("extractJSONType(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// ---------- streamGuard 定时器 ----------

func TestStreamGuard_StallAbort(t *testing.T) {
	sg := newStreamGuard(60 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	defer cancel()
	sg.arm(cancel)

	// 只喂开场事件,不喂内容 → 到点应判开场死
	sg.feed([]byte("event: message_start\n" + `data: {"type":"message_start"}` + "\n\n"))
	sg.feed([]byte("event: ping\n" + `data: {"type":"ping"}` + "\n\n"))

	deadline := time.After(500 * time.Millisecond)
	for !sg.abortedNow() {
		select {
		case <-deadline:
			t.Fatal("stall 超时后应判定开场死")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestStreamGuard_ReleaseOnContent(t *testing.T) {
	sg := newStreamGuard(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	defer cancel()
	sg.arm(cancel)

	sg.feed([]byte("event: message_start\n" + `data: {"type":"message_start"}` + "\n\n"))
	sg.feed([]byte("event: content_block_delta\n" + `data: {"type":"content_block_delta"}` + "\n\n"))
	if sg.abortedNow() {
		t.Fatal("内容已到达,不应判开场死")
	}
	time.Sleep(150 * time.Millisecond) // 越过 stall 窗口
	if sg.abortedNow() {
		t.Error("放行后 stall 定时器应已停,不应再判开场死")
	}
}

func TestStreamGuard_NoFeedNoAbort(t *testing.T) {
	sg := newStreamGuard(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	defer cancel()
	sg.arm(cancel)
	time.Sleep(120 * time.Millisecond)
	if sg.abortedNow() {
		t.Error("未 feed(非流式/无响应体)不应武装定时器,不应判开场死")
	}
}

// guard 放行时应触发 holder.releaseHold(groupWriter commit + 回放)
func TestStreamGuard_HolderReplay(t *testing.T) {
	sg := newStreamGuard(60 * time.Millisecond)
	rec := httptest.NewRecorder()
	gw := newGroupWriter(rec, []int{})
	gw.Header().Set("Content-Type", "text/event-stream")
	gw.WriteHeader(200)
	sg.holder = gw

	sg.feed([]byte("event: message_start\n" + `data: {"type":"message_start"}` + "\n\n"))
	sg.feed([]byte("event: content_block_start\n" + `data: {"type":"content_block_start"}` + "\n\n"))

	if !gw.committed {
		t.Fatal("内容事件到达后应已放行并 commit")
	}
}

// ---------- groupWriter hold ----------

func TestGroupWriter_HoldThenRelease(t *testing.T) {
	rec := httptest.NewRecorder()
	gw := newGroupWriter(rec, []int{})
	gw.Header().Set("Content-Type", "text/event-stream")
	gw.WriteHeader(200)
	if !gw.holdActive {
		t.Fatal("2xx SSE 应启用 hold")
	}

	// 开场事件被 hold
	gw.Write([]byte("event: message_start\n\n"))
	if gw.committed {
		t.Error("hold 中不应 commit")
	}
	if rec.Body.Len() > 0 {
		t.Error("hold 中不应有字节到达底层")
	}

	// 放行:缓存回放 + 后续直通
	gw.releaseHold()
	if !gw.committed {
		t.Fatal("放行后应 commit")
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Error("放行后缓存的字节应回放到底层")
	}
	gw.Write([]byte("event: content_block_delta\n\n"))
	if !strings.Contains(rec.Body.String(), "content_block_delta") {
		t.Error("放行后写入应直通底层")
	}
}

func TestGroupWriter_HoldIntercept401(t *testing.T) {
	// 非 2xx 不 hold;401 命中 on_status 拦截并记录错误体
	rec := httptest.NewRecorder()
	gw := newGroupWriter(rec, []int{401})
	gw.Header().Set("Content-Type", "text/event-stream")
	gw.WriteHeader(401)
	if gw.holdActive {
		t.Fatal("非 2xx 不应启用 hold")
	}
	gw.Write([]byte(`{"error":{"code":"1000"}}`))
	if !gw.intercepted {
		t.Fatal("401 命中 on_status 应拦截")
	}
	if rec.Body.Len() > 0 {
		t.Error("拦截的字节不应到达底层")
	}
	if !strings.Contains(string(gw.interceptBody), "1000") {
		t.Error("拦截时应记录错误体供 1302/1000 判定")
	}
}

// ---------- shouldRetrySameMember ----------

func TestShouldRetrySameMember(t *testing.T) {
	old := sameMemberRetryMaxDelay
	sameMemberRetryMaxDelay = 10 * time.Second
	defer func() { sameMemberRetryMaxDelay = old }()

	cases := []struct {
		name   string
		status int
		tryNum int
		delay  time.Duration
		want   bool
	}{
		{"瞬时502可重试", 502, 0, 100 * time.Millisecond, true},
		{"长挂502不重试", 502, 0, 30 * time.Second, false},
		{"429不重试", 429, 0, 100 * time.Millisecond, false},
		{"401不重试", 401, 0, 100 * time.Millisecond, false},
		{"第二回合不重试", 502, 1, 100 * time.Millisecond, false},
	}
	for _, c := range cases {
		if got := shouldRetrySameMember(c.status, c.tryNum, c.delay); got != c.want {
			t.Errorf("%s: shouldRetrySameMember(%d,%d,%v)=%v, 期望 %v",
				c.name, c.status, c.tryNum, c.delay, got, c.want)
		}
	}
}

// ---------- 端到端:group 路由 × 开场死换人 ----------

// mixedAuthBackend 单一后端按 Authorization 分流(复用现有 E2E 测试的模式):
//   - sk-m1 → 开场死:message_start+ping 后挂住 2s
//   - 其他  → 正常 anthropic 完整流
func mixedStillbornBackend(stillbornHang time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if strings.Contains(r.Header.Get("Authorization"), "sk-m1") {
			fmt.Fprint(w, "event: message_start\n"+`data: {"type":"message_start"}`+"\n\n")
			fmt.Fprint(w, "event: ping\n"+`data: {"type":"ping"}`+"\n\n")
			fl.Flush()
			select {
			case <-r.Context().Done(): // guard 取消上游 → 退出
			case <-time.After(stillbornHang):
			}
			return
		}
		fmt.Fprint(w, "event: message_start\n"+`data: {"type":"message_start"}`+"\n\n")
		fmt.Fprint(w, "event: content_block_delta\n"+`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"from-m2"}}`+"\n\n")
		fmt.Fprint(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
		fl.Flush()
	}))
}

func TestGroupE2E_StillbornFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过集成测试")
	}
	oldStall := streamFirstContentStall
	streamFirstContentStall = 150 * time.Millisecond
	defer func() { streamFirstContentStall = oldStall }()

	backend := mixedStillbornBackend(2 * time.Second)
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-m1", Header: "Authorization", Prefix: "Bearer "}
	ks.configs["m2"] = KeyConfig{Key: "sk-m2", Header: "Authorization", Prefix: "Bearer "}
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1", "m2"}, OnStatus: []int{502}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)
	if settingsMgr == nil {
		settingsMgr = newSettingsManager()
	}
	stats := newStatsCollector()
	us := newUsageStats()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/g/") {
			handleGroupRoutePrefix(w, req, ks, stats, us)
			return
		}
		http.Error(w, "no route", 500)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxy.URL + "/g/pool/" + backend.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("期望最终状态 200(来自 m2),得到 %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			got = append(got, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("客户端没收到任何内容")
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, `"type":"ping"`) {
		t.Errorf("m1 开场死的 ping 残骸不应到达客户端: %s", joined)
	}
	if !strings.Contains(joined, "from-m2") {
		t.Errorf("应收到 m2 的内容: %s", joined)
	}

	// m1 进冷却
	if !ks.groupMgr.memberStatus("m1").IsCooling {
		t.Error("m1 开场死后应被标记冷却")
	}
}
