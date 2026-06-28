// llm-http-proxy 性能基准测试。
//
// 对比"直连后端" vs "经代理转发" 的吞吐与延迟开销,
// 并测 WebSocket 的往返吞吐。全部本地自包含,零外网依赖。
//
// 运行:
//
//	go test -bench=. -benchmem -benchtime=2s .
//	go test -bench=. -benchmem -count=5 .     # 多轮取稳定值
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// benchBackend 是一个最小后端:接收 body,固定返回一小段 JSON。
func benchBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // 读完 body,丢弃
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
}

// benchClient 返回连接池化的 HTTP 客户端。
// 关键:复用 Transport 让 keep-alive 连接池生效,避免短连接耗尽临时端口。
// MaxIdleConnsPerHost 默认只有 2,压测时调高以充分复用连接。
func benchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression:  true,
			MaxIdleConnsPerHost: 256,
		},
	}
}

// benchPayload 是每次请求发送的 JSON body。
var benchPayload = []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`)

// BenchmarkDirect 直连后端,作为对照组。
func BenchmarkDirect(b *testing.B) {
	backend := benchBackend()
	defer backend.Close()

	client := benchClient()
	url := backend.URL + "/chat"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, err := client.Post(url, "application/json", bytes.NewReader(benchPayload))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy 经代理转发同样的请求(串行)。
func BenchmarkProxy(b *testing.B) {
	backend := benchBackend()
	defer backend.Close()

	proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", nil, nil))
	defer proxy.Close()

	client := benchClient()
	url := proxy.URL + "/" + backend.URL + "/chat"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, err := client.Post(url, "application/json", bytes.NewReader(benchPayload))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxyConcurrent 经代理转发,并发跑(模拟多客户端同时请求)。
func BenchmarkProxyConcurrent(b *testing.B) {
	backend := benchBackend()
	defer backend.Close()

	proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", nil, nil))
	defer proxy.Close()

	client := benchClient()
	url := proxy.URL + "/" + backend.URL + "/chat"

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Post(url, "application/json", bytes.NewReader(benchPayload))
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// BenchmarkWebSocket 测 WS 双向隧道的往返吞吐。
// 复用一条长连接多次收发,贴近真实 WS 用法,也避免反复建连耗尽端口。
func BenchmarkWebSocket(b *testing.B) {
	wsBackend := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		io.Copy(ws, ws) // echo
	}))
	defer wsBackend.Close()

	proxy := httptest.NewServer(newProxyHandler(newStatsCollector(), nil, "", nil, nil))
	defer proxy.Close()

	proxyWS := "ws:" + strings.TrimPrefix(proxy.URL, "http:")
	wsTarget := "ws:" + strings.TrimPrefix(wsBackend.URL, "http:")
	target := proxyWS + "/" + wsTarget

	// 建立一条长连接,在循环里反复收发
	cfg, err := websocket.NewConfig(target, "http://localhost/")
	if err != nil {
		b.Fatal(err)
	}
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	msg := []byte("benchmark-ping")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(msg); err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := conn.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
