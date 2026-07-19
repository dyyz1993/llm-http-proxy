package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// timedSSEBackend 启动一个 SSE 后端: 发 n 个 chunk, 每个 chunk 间隔 interval,
// 在每个 chunk 的 data: 行里带上后端"发送时刻"(ms 级单调时钟)。
// 客户端读取时再打一个"到达时刻",两者差值即为代理引入的缓冲延迟。
func timedSSEBackend(n int, interval time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		start := time.Now()
		for i := 0; i < n; i++ {
			if i > 0 {
				time.Sleep(interval)
			}
			// sentAt = 距后端响应开始的毫秒数
			sentAt := time.Since(start).Milliseconds()
			fmt.Fprintf(w, "data: chunk-%d sentAt=%d\n\n", i, sentAt)
			fl.Flush()
		}
	}))
}

// labeledSSEBackend 发 SSE chunk, 每个 chunk 带 label 标记(用于区分是哪个后端发的)。
// statusCode 非 200 时先写该状态码再流式发 body。
func labeledSSEBackend(label string, n int, interval time.Duration, statusCode int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		if statusCode != 200 && statusCode != 0 {
			w.WriteHeader(statusCode)
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				time.Sleep(interval)
			}
			fmt.Fprintf(w, "data: label=%s idx=%d\n\n", label, i)
			fl.Flush()
		}
	}))
}

// measureSSETiming 通过 proxyURL 请求后端, 返回:
//   - chunks:       每条 data: 行的内容
//   - arrivalMs:    每条 chunk 到达客户端的相对毫秒(从读到第一条算起)
//   - spreadMs:     最后一条到达 - 第一条到达(越大表示流式越好; 越接近 0 表示被 buffer)
func measureSSETiming(t *testing.T, client *http.Client, url string) (chunks []string, arrivalMs []int64, spreadMs int64) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 不是 SSE: %q", ct)
	}

	var t0 time.Time
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		now := time.Now()
		if t0.IsZero() {
			t0 = now
		}
		chunks = append(chunks, line)
		arrivalMs = append(arrivalMs, now.Sub(t0).Milliseconds())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(arrivalMs) > 0 {
		spreadMs = arrivalMs[len(arrivalMs)-1] - arrivalMs[0]
	}
	return
}

// TestSSETiming_KRoute 确认 /k/ 别名路由在 SSE 下是流式的:
// 5 个间隔 100ms 的 chunk, 最后一条到达时刻应该明显晚于第一条(流式)。
func TestSSETiming_KRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in short mode")
	}

	backend := timedSSEBackend(5, 100*time.Millisecond)
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["alias"] = KeyConfig{Key: "sk-fake", Header: "Authorization", Prefix: "Bearer "}
	if settingsMgr == nil {
		settingsMgr = newSettingsManager()
	}

	stats := newStatsCollector()
	us := newUsageStats()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/k/"):
			handleKeyRoute(w, req, ks, stats, us)
		default:
			http.Error(w, "no route", 500)
		}
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	chunks, arrival, spread := measureSSETiming(t, noCompressClient,
		proxy.URL+"/k/alias/"+backend.URL+"/sse")

	if len(chunks) != 5 {
		t.Fatalf("收到 %d 个 chunk, 期望 5", len(chunks))
	}

	t.Logf("/k/ 各 chunk 到达时刻(ms): %v", arrival)
	t.Logf("/k/ 首→末 spread = %dms (流式期望接近 400ms)", spread)

	// 流式判定: 首 chunk 与末 chunk 之间应该有可观的延迟。
	// 5 chunk × 100ms 间隔 = 后端发完用 400ms, 真流式下末 chunk 至少比首 chunk 晚 ~300ms。
	if spread < 250 {
		t.Errorf("/k/ 疑似被 buffer: 首→末 spread 仅 %dms, 期望 ≥300ms. arrival=%v",
			spread, arrival)
	}
}

// TestSSETiming_GRoute 验证当前 /g/ group 路由对 SSE 的处理。
// 这是诊断测试: 它会打印实际到达时序, 不强制 pass/fail。
// 如果 spread 接近 0ms, 说明响应被 buffer 成一坨, 这正是 tool_call delta 不流式的根因。
func TestSSETiming_GRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in short mode")
	}

	backend := timedSSEBackend(5, 100*time.Millisecond)
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["m1"] = KeyConfig{Key: "sk-fake", Header: "Authorization", Prefix: "Bearer "}
	ks.groups = map[string]GroupConfig{
		"pool": {Members: []string{"m1"}, OnStatus: []int{502}, Cooldown: "1m"},
	}
	ks.groupMgr.updateGroups(ks.groups)
	if settingsMgr == nil {
		settingsMgr = newSettingsManager()
	}

	stats := newStatsCollector()
	us := newUsageStats()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/g/"):
			handleGroupRoutePrefix(w, req, ks, stats, us)
		default:
			http.Error(w, "no route", 500)
		}
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	chunks, arrival, spread := measureSSETiming(t, noCompressClient,
		proxy.URL+"/g/pool/"+backend.URL+"/sse")

	if len(chunks) != 5 {
		t.Fatalf("收到 %d 个 chunk, 期望 5", len(chunks))
	}

	t.Logf("/g/ 各 chunk 到达时刻(ms): %v", arrival)
	t.Logf("/g/ 首→末 spread = %dms (流式期望 ~400ms, 被 buffer 会接近 0ms)", spread)

	// 回归判定: 修复后 /g/ 必须是流式的。
	// 5 chunk × 100ms 间隔 → 真·流式下 spread 应接近 400ms。
	// 若 spread < 100ms, 说明 groupWriter 的延迟 commit 被改回了全量 buffer。
	if spread < 100 {
		t.Errorf("/g/ SSE 被 buffer 了: 首→末 spread 仅 %dms, 期望 ~400ms. "+
			"arrival=%v (回归到 bufferedWriter 的 bug)", spread, arrival)
	}
}

// TestGroupRoute_E2E_SSEStillSwitchesOnStatus 是关键的回归测试:
// 验证 groupWriter 流式改造**没有破坏**换人能力。
// 场景: group 路由把同一个 SSE 后端(根据 Authorization 区分)作为目标。
//   - 用 sk-m1 的成员访问时,后端返回 502(命中 on_status) + 1 个 SSE chunk
//   - 用 sk-m2 的成员访问时,后端返回 200 + 3 个 SSE chunk
//
// 期望: group 换人到 m2, 客户端只收到 m2 的内容, m1 被标记冷却。
func TestGroupRoute_E2E_SSEStillSwitchesOnStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// 单一后端, 根据 Authorization header 返回不同响应
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if strings.Contains(auth, "sk-m1") {
			// 模拟 m1 上游错误: 502 + SSE body(命中 on_status)
			w.WriteHeader(502)
			fmt.Fprintf(w, "data: label=m1 idx=0\n\n")
			fl.Flush()
			return
		}
		// m2 (正常 SSE)
		for i := 0; i < 3; i++ {
			if i > 0 {
				time.Sleep(20 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: label=m2 idx=%d\n\n", i)
			fl.Flush()
		}
	}))
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

	// 请求 group, 目标是 backend 的 SSE 路径
	url := proxy.URL + "/g/pool/" + backend.URL + "/sse"
	resp, err := noCompressClient.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 最终响应应该是 m2 的(200),不是 m1 的 502
	if resp.StatusCode != 200 {
		t.Errorf("期望最终状态码 200(来自 m2), 得到 %d", resp.StatusCode)
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

	t.Logf("客户端收到的 chunks: %v", got)

	// 关键断言: 客户端**只能**看到 m2 的内容, m1 的内容必须被丢弃
	for _, c := range got {
		if strings.Contains(c, "label=m1") {
			t.Errorf("客户端收到了 m1(应被换人丢弃)的内容: %s", c)
		}
	}
	if len(got) == 0 {
		t.Fatal("客户端没收到任何 chunk")
	}
	if !strings.Contains(got[0], "label=m2") {
		t.Errorf("期望首个 chunk 来自 m2, 得到 %s", got[0])
	}

	// m1 应该被标记冷却
	m1Status := ks.groupMgr.memberStatus("m1")
	if !m1Status.IsCooling {
		t.Error("m1 返回 502 后应被标记冷却")
	}
}
