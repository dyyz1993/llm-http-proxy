// glm-proxy 的测试。全部使用本地 echo 后端,零外网依赖,确定性可重跑。
//
// 覆盖类型:
//   - 普通 HTTP:GET / POST+JSON / 表单 urlencoded / 文件上传 multipart / PUT / DELETE
//   - 自定义 header 透传(含 Authorization)
//   - 响应头透传
//   - SSE 流式
//   - WebSocket(wss 需证书,这里测 ws:// 明文隧道)
//   - 错误输入(空路径)
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// --- 本地 echo 后端 -------------------------------------------------------

// echoBackend 返回一个 HTTP 测试服务器,它把请求的关键信息回显为 JSON,
// 同时在响应头里放一个自定义标记,方便测试响应头透传。
func echoBackend() *httptest.Server {
	mux := http.NewServeMux()

	// echo 接口:回显 method/path/query/headers/body
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// 读取 query
		query := r.URL.Query().Encode()

		// 收集 header(用客户端发来的原始值)
		headers := map[string]string{}
		for k, vs := range r.Header {
			if len(vs) > 0 {
				headers[k] = vs[0]
			}
		}

		w.Header().Set("X-Echo-Marker", "from-backend") // 响应头标记
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)

		resp := map[string]interface{}{
			"method":  r.Method,
			"path":    r.URL.Path,
			"query":   query,
			"headers": headers,
			"body":    string(body),
		}
		json.NewEncoder(w).Encode(resp)
	})

	// SSE 接口:逐行推送 data: 行
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			fl.Flush()
		}
	})

	// 二进制回显接口:把收到的 body 原样回写(含正确 Content-Type),
	// 用于验证二进制 body 不被损坏。
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body) // 原样回吐
	})

	return httptest.NewServer(mux)
}

// noCompressClient 是一个禁用自动压缩的 HTTP 客户端。
// Go 的 DefaultClient 会在出站请求里自动加 "Accept-Encoding: gzip",
// 这会干扰"代理是否追加 header"的判定。测试统一用它,排除噪音。
var noCompressClient = &http.Client{
	Transport: &http.Transport{DisableCompression: true},
}

// wsEchoBackend 返回一个 WebSocket echo 测试服务器。
func wsEchoBackend() *httptest.Server {
	handler := websocket.Handler(func(ws *websocket.Conn) {
		io.Copy(ws, ws) // 收到什么就回什么
	})
	return httptest.NewServer(handler)
}

// --- 公共:用代理封装后端,返回代理 URL ------------------------------------

// startProxy 启动测试服务器,路由与 main 一致(/__version、/__stats + 代理)。
func startProxy(t *testing.T) *httptest.Server {
	t.Helper()
	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/__version":
			versionHandler(w, req)
			return
		case "/__stats":
			statsHandler(stats).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats).ServeHTTP(w, req)
	})
	return httptest.NewServer(mux)
}

// proxyURL 把后端 URL 拼到代理路径上:proxy + "/" + backend。
func proxyURL(proxy, backend string) string {
	return proxy + "/" + backend
}

// --- 测试用例 -------------------------------------------------------------

func TestGET(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxyURL(proxy.URL, backend.URL+"/get?x=1&y=2"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d,期望 200", resp.StatusCode)
	}
	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["method"] != "GET" {
		t.Errorf("method = %v,期望 GET", got["method"])
	}
	if got["query"] != "x=1&y=2" {
		t.Errorf("query = %v,期望 x=1&y=2", got["query"])
	}
	if got["path"] != "/get" {
		t.Errorf("path = %v,期望 /get", got["path"])
	}
}

func TestPOSTJSON(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	body := `{"hello":"world","n":42}`
	resp, err := noCompressClient.Post(
		proxyURL(proxy.URL, backend.URL+"/api"),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["body"] != body {
		t.Errorf("body = %v,期望 %s", got["body"], body)
	}
	hdrs, _ := got["headers"].(map[string]interface{})
	if hdrs["Content-Type"] != "application/json" {
		t.Errorf("Content-Type 未透传: %v", hdrs["Content-Type"])
	}
}

func TestFormURLEncoded(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	form := url.Values{"name": {"张三"}, "age": {"20"}}
	resp, err := http.PostForm(proxyURL(proxy.URL, backend.URL+"/form"), form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["body"] != form.Encode() {
		t.Errorf("表单 body = %v,期望 %s", got["body"], form.Encode())
	}
}

func TestMultipartUpload(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	// 构造 multipart body
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	field, _ := mw.CreateFormField("desc")
	field.Write([]byte("一个文件"))
	file, _ := mw.CreateFormFile("file", "test.txt")
	fileContent := []byte("hello-file-content\n")
	file.Write(fileContent)
	mw.Close()

	resp, err := noCompressClient.Post(
		proxyURL(proxy.URL, backend.URL+"/upload"),
		mw.FormDataContentType(),
		&buf,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	bodyStr, _ := got["body"].(string)
	// multipart body 里应包含文件名和文件内容
	if !strings.Contains(bodyStr, "test.txt") {
		t.Errorf("multipart body 缺文件名 test.txt: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, string(fileContent)) {
		t.Errorf("multipart body 缺文件内容: %s", bodyStr)
	}
}

func TestPUT(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	req, _ := http.NewRequest("PUT",
		proxyURL(proxy.URL, backend.URL+"/item"),
		strings.NewReader(`{"update":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["method"] != "PUT" {
		t.Errorf("method = %v,期望 PUT", got["method"])
	}
}

func TestDELETE(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	req, _ := http.NewRequest("DELETE",
		proxyURL(proxy.URL, backend.URL+"/item/5"), nil)
	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["method"] != "DELETE" {
		t.Errorf("method = %v,期望 DELETE", got["method"])
	}
}

func TestCustomHeaderPassthrough(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	// 关键:验证 Authorization 等自定义头原样透传,代理不追加额外头
	req, _ := http.NewRequest("GET",
		proxyURL(proxy.URL, backend.URL+"/auth"), nil)
	req.Header.Set("Authorization", "Bearer my-secret-token")
	req.Header.Set("X-Custom", "自定义值")
	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	hdrs, _ := got["headers"].(map[string]interface{})

	if hdrs["Authorization"] != "Bearer my-secret-token" {
		t.Errorf("Authorization 未透传: %v", hdrs["Authorization"])
	}
	if hdrs["X-Custom"] != "自定义值" {
		t.Errorf("X-Custom 未透传: %v", hdrs["X-Custom"])
	}
	// 代理不应追加 Accept-Encoding: gzip
	if ae, ok := hdrs["Accept-Encoding"]; ok && strings.Contains(ae.(string), "gzip") {
		t.Errorf("代理追加了 Accept-Encoding: gzip,破坏透传: %v", ae)
	}
}

func TestResponseHeaderPassthrough(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxyURL(proxy.URL, backend.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Echo-Marker") != "from-backend" {
		t.Errorf("响应头 X-Echo-Marker 未透传: %q",
			resp.Header.Get("X-Echo-Marker"))
	}
}

func TestSSE(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxyURL(proxy.URL, backend.URL+"/sse"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("SSE Content-Type 未透传: %q", resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	var chunks []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			chunks = append(chunks, line)
		}
	}
	if len(chunks) != 3 {
		t.Fatalf("收到 %d 个 SSE chunk,期望 3", len(chunks))
	}
	if chunks[0] != "data: chunk-0" || chunks[2] != "data: chunk-2" {
		t.Errorf("SSE 内容不对: %v", chunks)
	}
}

func TestWebSocket(t *testing.T) {
	wsBackend := wsEchoBackend()
	defer wsBackend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	// 代理监听是 http://,WS 客户端要用 ws:// 协议访问代理。
	// 目标后端 URL(ws://...)拼在代理路径后。
	// proxy.URL 形如 http://127.0.0.1:port -> 改成 ws://127.0.0.1:port
	proxyWS := "ws:" + strings.TrimPrefix(proxy.URL, "http:")
	wsTarget := "ws:" + strings.TrimPrefix(wsBackend.URL, "http:")
	proxyTarget := proxyWS + "/" + wsTarget

	// 用 golang.org/x/net/websocket 客户端
	cfg, err := websocket.NewConfig(proxyTarget, "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	defer conn.Close()

	messages := []string{"WS-测试-1", "second", "中文消息✓"}
	for _, msg := range messages {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("发送失败: %v", err)
		}
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("接收失败: %v", err)
		}
		got := string(buf[:n])
		if got != msg {
			t.Errorf("回显不匹配: got %q,期望 %q", got, msg)
		}
	}
}

func TestEmptyPath(t *testing.T) {
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("空路径状态码 = %d,期望 400", resp.StatusCode)
	}
}

// TestVersionEndpoint 验证 /__version 返回版本/编译时间/启动时间/运行时长。
func TestVersionEndpoint(t *testing.T) {
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxy.URL + "/__version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d,期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q,期望 application/json", ct)
	}

	var info versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	// version/buildTime 在测试时是默认值 "dev"/"unknown"
	if info.Version == "" {
		t.Error("version 为空")
	}
	if info.BuildTime == "" {
		t.Error("build_time 为空")
	}
	// start_time 必须是合法的 RFC3339
	if _, err := time.Parse(time.RFC3339, info.StartTime); err != nil {
		t.Errorf("start_time %q 不是合法 RFC3339: %v", info.StartTime, err)
	}
	// uptime 必须非空且包含时间单位
	if info.Uptime == "" || !strings.ContainsAny(info.Uptime, "smh") {
		t.Errorf("uptime %q 不合法", info.Uptime)
	}
}

// TestVersionMethodNotAllowed 验证 /__version 只接受 GET。
func TestVersionMethodNotAllowed(t *testing.T) {
	proxy := startProxy(t)
	defer proxy.Close()

	resp, err := noCompressClient.Post(proxy.URL+"/__version", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST /__version 状态码 = %d,期望 405", resp.StatusCode)
	}
}

// 额外:大体积二进制透传校验,确保 body 不被损坏。
func TestBinaryBody(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	// 生成 256KB 随机二进制(含各种字节值,含 0x00)
	payload := make([]byte, 256*1024)
	rand.Read(payload)
	want := sha256.Sum256(payload)

	resp, err := noCompressClient.Post(
		proxyURL(proxy.URL, backend.URL+"/bin"),
		"application/octet-stream",
		bytes.NewReader(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("二进制 body 长度变化: got %d,期望 %d", len(got), len(payload))
	}
	gotHash := sha256.Sum256(got)
	if gotHash != want {
		t.Errorf("二进制 body 哈希不匹配,内容被损坏")
	}
}

// --- 统计采集测试 --------------------------------------------------------

// TestMaskKey 验证 key 掩码规则:保留前缀(到首个'-')+ 后4位,中间 *。
func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		// OpenAI: sk- 前缀。19 字符 - 3(prefix) - 4(tail) = 12 个 *
		{"sk-abcd1234efgh5678", "sk-************5678"},
		// Claude: 第一个 '-' 后 prefix=sk-。
		{"sk-ant-aaa111bbb222ccc333ddd444", "sk-************************d444"},
		// GLM: 无 '-',prefix=前4位。25 字符 - 4 - 4 = 17 个 *
		{"f8dcf55ef4cb.lAwRTT5GCxS4", "f8dc*****************CxS4"},
		{"short", "*****"},            // <=8 全掩码
		{"12345678", "********"},      // 恰好 8 全掩码
		{"123456789", "1234****6789"}, // 9 字符,空隙=1≤4,用 4 个 *
	}
	for _, c := range cases {
		got := maskKey(c.in)
		if got != c.want {
			t.Errorf("maskKey(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

// TestMaskKeyShort 确保 key 过短时不会泄露。
func TestMaskKeyShort(t *testing.T) {
	for _, k := range []string{"a", "ab", "abc", "1234", "12345678"} {
		m := maskKey(k)
		// 过短的 key 全部掩码,不应出现任何明文字符
		if strings.Trim(m, "*") != "" {
			t.Errorf("短 key %q 掩码后 %q 仍有明文", k, m)
		}
	}
}

// TestExtractKey 验证三种 header 位置的 key 提取。
func TestExtractKey(t *testing.T) {
	// Authorization: Bearer
	r1, _ := http.NewRequest("POST", "/", nil)
	r1.Header.Set("Authorization", "Bearer sk-test1234567")
	if k, ok := extractKey(r1); !ok || k != "sk-test1234567" {
		t.Errorf("Bearer 提取: got %q ok=%v", k, ok)
	}

	// x-api-key (Claude)
	r2, _ := http.NewRequest("POST", "/", nil)
	r2.Header.Set("x-api-key", "sk-ant-xyz")
	if k, ok := extractKey(r2); !ok || k != "sk-ant-xyz" {
		t.Errorf("x-api-key 提取: got %q ok=%v", k, ok)
	}

	// api-key (Azure)
	r3, _ := http.NewRequest("POST", "/", nil)
	r3.Header.Set("api-key", "azure-key-123")
	if k, ok := extractKey(r3); !ok || k != "azure-key-123" {
		t.Errorf("api-key 提取: got %q ok=%v", k, ok)
	}

	// 无 key
	r4, _ := http.NewRequest("POST", "/", nil)
	if _, ok := extractKey(r4); ok {
		t.Errorf("无 key 时应返回 false")
	}
}

// TestStatsRecordAndAggregate 验证经代理的请求被正确统计到 /__stats。
func TestStatsRecordAndAggregate(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/__stats" {
			statsHandler(stats).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats).ServeHTTP(w, req)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	// 用 Authorization: Bearer 发 2 次请求
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST",
			proxyURL(proxy.URL, backend.URL+"/api"), strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-abcd1234efgh5678")
		resp, err := noCompressClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// 用 x-api-key 发 1 次(Claude 风格)
	req, _ := http.NewRequest("POST",
		proxyURL(proxy.URL, backend.URL+"/api"), strings.NewReader("{}"))
	req.Header.Set("x-api-key", "sk-ant-aaa111bbb222ccc")
	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 拉 /__stats
	resp, err = noCompressClient.Get(proxy.URL + "/__stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var snap map[string]*ipStats
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}

	// 应该只有一个 IP(127.0.0.1),下面有两个掩码 key
	if len(snap) != 1 {
		t.Fatalf("IP 数 = %d,期望 1: %+v", len(snap), snap)
	}
	// 找到那个 IP 的条目
	var is *ipStats
	for _, v := range snap {
		is = v
	}
	if len(is.Keys) != 2 {
		t.Fatalf("掩码 key 数 = %d,期望 2: %+v", len(is.Keys), is.Keys)
	}
	// Bearer key 应计数 2
	bearerMasked := maskKey("sk-abcd1234efgh5678")
	if ke := is.Keys[bearerMasked]; ke == nil || ke.Count != 2 {
		t.Errorf("Bearer key 计数: %+v,期望 2", ke)
	}
	// x-api-key 应计数 1
	claudeMasked := maskKey("sk-ant-aaa111bbb222ccc")
	if ke := is.Keys[claudeMasked]; ke == nil || ke.Count != 1 {
		t.Errorf("Claude key 计数: %+v,期望 1", ke)
	}
}

// TestStatsNoPlaintextKey 确保日志和统计里不含明文 key。
func TestStatsNoPlaintextKey(t *testing.T) {
	// 构造一个请求,原始 key 较长
	r, _ := http.NewRequest("POST", "/", nil)
	plainKey := "sk-supersecret-key-1234567890abcdef"
	r.Header.Set("Authorization", "Bearer "+plainKey)

	masked := maskedKeyFromRequest(r)
	if masked == plainKey {
		t.Fatal("掩码结果等于明文 key!")
	}
	if strings.Contains(masked, "supersecret") {
		t.Fatalf("掩码结果 %q 包含明文片段", masked)
	}
	if !strings.Contains(masked, "*") {
		t.Fatalf("掩码结果 %q 不含 *", masked)
	}
}

// TestStatsFirstSeen 验证 first_seen 字段:首次访问时记录,后续不变,且早于 last_seen。
func TestStatsFirstSeen(t *testing.T) {
	stats := newStatsCollector()

	// 第一次记录
	stats.record("10.0.0.1", "sk-****1111", "host.com", 200)
	snap1 := stats.snapshot()
	ke1 := snap1["10.0.0.1"].Keys["sk-****1111"]
	if ke1.FirstSeen.IsZero() {
		t.Fatal("first_seen 未记录")
	}
	first := ke1.FirstSeen

	// 等待一小段,再次记录,first_seen 不应变
	time.Sleep(20 * time.Millisecond)
	stats.record("10.0.0.1", "sk-****1111", "host.com", 200)
	snap2 := stats.snapshot()
	ke2 := snap2["10.0.0.1"].Keys["sk-****1111"]

	if !ke2.FirstSeen.Equal(first) {
		t.Errorf("first_seen 变了: 第一次 %v, 第二次 %v", first, ke2.FirstSeen)
	}
	if !ke2.LastSeen.After(ke2.FirstSeen) {
		t.Errorf("last_seen(%v) 不晚于 first_seen(%v)", ke2.LastSeen, ke2.FirstSeen)
	}
	if ke2.Count != 2 {
		t.Errorf("count = %d, 期望 2", ke2.Count)
	}
}

// TestStatsPersistRoundTrip 验证持久化:save 到文件 -> 新 collector load -> 数据一致。
func TestStatsPersistRoundTrip(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "stats.json")
	t.Cleanup(func() { os.Remove(tmpFile) })

	// 写入一些数据
	s1 := newStatsCollector()
	s1.record("10.0.0.1", "sk-****1111", "a.com", 200)
	s1.record("10.0.0.1", "sk-****1111", "a.com", 200)
	s1.record("10.0.0.2", "sk-****2222", "b.com", 500)
	if err := s1.save(tmpFile); err != nil {
		t.Fatalf("save 失败: %v", err)
	}

	// 新 collector 从文件加载
	s2 := newStatsCollector()
	if err := s2.load(tmpFile); err != nil {
		t.Fatalf("load 失败: %v", err)
	}

	snap := s2.snapshot()
	if len(snap) != 2 {
		t.Fatalf("load 后 IP 数 = %d, 期望 2", len(snap))
	}
	ke := snap["10.0.0.1"].Keys["sk-****1111"]
	if ke == nil || ke.Count != 2 {
		t.Errorf("10.0.0.1/sk-****1111 count = %v, 期望 2", ke)
	}
	if ke.LastTarget != "a.com" {
		t.Errorf("last_target = %q, 期望 a.com", ke.LastTarget)
	}
	ke2 := snap["10.0.0.2"].Keys["sk-****2222"]
	if ke2 == nil || ke2.LastStatus != 500 {
		t.Errorf("10.0.0.2/sk-****2222 status = %v, 期望 500", ke2)
	}
}

// TestStatsLoadMissingFile 验证文件不存在时 load 不报错(首次启动)。
func TestStatsLoadMissingFile(t *testing.T) {
	s := newStatsCollector()
	err := s.load("/nonexistent/path/stats.json")
	if err != nil {
		t.Errorf("文件不存在时 load 应返回 nil,得到: %v", err)
	}
	if len(s.data) != 0 {
		t.Errorf("load 后数据非空: %+v", s.data)
	}
}

// TestStatsStatusCounts 验证状态码计数 + 成功率派生。
func TestStatsStatusCounts(t *testing.T) {
	stats := newStatsCollector()
	// 3 次 200,1 次 500
	stats.record("10.0.0.1", "sk-****0001", "a.com", 200)
	stats.record("10.0.0.1", "sk-****0001", "a.com", 200)
	stats.record("10.0.0.1", "sk-****0001", "a.com", 200)
	stats.record("10.0.0.1", "sk-****0001", "a.com", 500)

	snap := stats.snapshot()
	ke := snap["10.0.0.1"].Keys["sk-****0001"]
	if ke.Count != 4 {
		t.Errorf("count = %d, 期望 4", ke.Count)
	}
	if ke.StatusCounts[200] != 3 {
		t.Errorf("status_counts[200] = %d, 期望 3", ke.StatusCounts[200])
	}
	if ke.StatusCounts[500] != 1 {
		t.Errorf("status_counts[500] = %d, 期望 1", ke.StatusCounts[500])
	}

	// 成功率 = 2xx/总数 = 3/4 = 0.75
	byIP := statsByIP(snap)
	if r := byIP["10.0.0.1"].SuccessRate; r < 0.74 || r > 0.76 {
		t.Errorf("success_rate = %v, 期望 ~0.75", r)
	}
}

// TestStatsWindow 验证时间窗口:record 后 hoursSnapshot 应有计数。
func TestStatsWindow(t *testing.T) {
	stats := newStatsCollector()
	for i := 0; i < 5; i++ {
		stats.record("10.0.0.1", "sk-****0001", "a.com", 200)
	}
	hours := stats.hoursSnapshot(3)
	if len(hours) != 3 {
		t.Fatalf("hours 长度 = %d, 期望 3", len(hours))
	}
	// 最后一桶(当前小时)应有 5 次
	last := hours[len(hours)-1]
	if last.Count != 5 {
		t.Errorf("当前小时 count = %d, 期望 5", last.Count)
	}
}

// TestStatsTopN 验证 top=N 只返回调用最多的 N 个。
func TestStatsTopN(t *testing.T) {
	stats := newStatsCollector()
	// IP-A: 10 次, IP-B: 3 次, IP-C: 1 次
	for i := 0; i < 10; i++ {
		stats.record("10.0.0.1", "sk-****0001", "a.com", 200)
	}
	for i := 0; i < 3; i++ {
		stats.record("10.0.0.2", "sk-****0002", "a.com", 200)
	}
	stats.record("10.0.0.3", "sk-****0003", "a.com", 200)

	snap := stats.snapshot()
	top2 := topN(snap, "ip", 2)
	if len(top2) != 2 {
		t.Fatalf("top2 长度 = %d, 期望 2", len(top2))
	}
	// 应包含调用最多的 10.0.0.1
	if _, ok := top2["10.0.0.1"]; !ok {
		t.Errorf("top2 应包含 10.0.0.1")
	}
	// 不应包含最少的 10.0.0.3
	if _, ok := top2["10.0.0.3"]; ok {
		t.Errorf("top2 不应包含 10.0.0.3(调用最少)")
	}
}

// TestStatsByKeyView 验证 by=key 反向视图:以 key 为顶层聚合 IP。
func TestStatsByKeyView(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/__stats" {
			statsHandler(stats).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats).ServeHTTP(w, req)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	// 同一个 key,从"两个不同 IP"调用(用 X-Forwarded-For 模拟)
	for _, fakeIP := range []string{"10.0.0.1", "10.0.0.2"} {
		req, _ := http.NewRequest("POST",
			proxyURL(proxy.URL, backend.URL+"/api"), strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-shared-key-1234567890")
		req.Header.Set("X-Forwarded-For", fakeIP)
		resp, err := noCompressClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// 拉 by=key 视图
	resp, err := noCompressClient.Get(proxy.URL + "/__stats?by=key")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var byKey map[string]*keyAggView
	if err := json.NewDecoder(resp.Body).Decode(&byKey); err != nil {
		t.Fatal(err)
	}

	masked := maskKey("sk-shared-key-1234567890")
	kv, ok := byKey[masked]
	if !ok {
		t.Fatalf("反向视图里找不到 key %q: %+v", masked, byKey)
	}
	// 这个 key 应该触发 2 个不同 IP
	if kv.DistinctIPs != 2 {
		t.Errorf("DistinctIPs = %d,期望 2: %+v", kv.DistinctIPs, kv)
	}
	if _, ok := kv.IPs["10.0.0.1"]; !ok {
		t.Errorf("反向视图里缺少 IP 10.0.0.1: %+v", kv.IPs)
	}
	if _, ok := kv.IPs["10.0.0.2"]; !ok {
		t.Errorf("反向视图里缺少 IP 10.0.0.2: %+v", kv.IPs)
	}
}

// TestStatsDistinctCount 验证去重统计字段(distinct_keys / distinct_ips)。
func TestStatsDistinctCount(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/__stats" {
			statsHandler(stats).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats).ServeHTTP(w, req)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	// IP-A 用 2 个不同 key,IP-B 用 1 个 key(和 IP-A 的一个相同)
	calls := []struct{ ip, key string }{
		{"10.0.0.1", "Bearer sk-aaa1112223334444"},
		{"10.0.0.1", "Bearer sk-bbb5556667778888"},
		{"10.0.0.2", "Bearer sk-aaa1112223334444"}, // 复用 IP-A 的第一个 key
	}
	for _, c := range calls {
		req, _ := http.NewRequest("POST",
			proxyURL(proxy.URL, backend.URL+"/api"), strings.NewReader("{}"))
		req.Header.Set("Authorization", c.key)
		req.Header.Set("X-Forwarded-For", c.ip)
		resp, err := noCompressClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// by=ip:应 2 个 IP,IP-A 有 2 个 distinct key
	resp, err := noCompressClient.Get(proxy.URL + "/__stats?by=ip")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var byIP map[string]*ipAggView
	json.NewDecoder(resp.Body).Decode(&byIP)
	if len(byIP) != 2 {
		t.Errorf("IP 数 = %d,期望 2", len(byIP))
	}
	if byIP["10.0.0.1"].DistinctKeys != 2 {
		t.Errorf("10.0.0.1 distinct_keys = %d,期望 2", byIP["10.0.0.1"].DistinctKeys)
	}

	// by=key:2 个不同 key,第一个 key(sk-aaa...))有 2 个 distinct IP
	resp2, err := noCompressClient.Get(proxy.URL + "/__stats?by=key")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var byKey map[string]*keyAggView
	json.NewDecoder(resp2.Body).Decode(&byKey)
	if len(byKey) != 2 {
		t.Errorf("key 数 = %d,期望 2", len(byKey))
	}
	sharedKey := maskKey("sk-aaa1112223334444")
	if byKey[sharedKey].DistinctIPs != 2 {
		t.Errorf("共享 key distinct_ips = %d,期望 2", byKey[sharedKey].DistinctIPs)
	}
}

// TestStatsFormatTable 验证 format=table 返回 ASCII 表格(非 JSON)。
func TestStatsFormatTable(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/__stats" {
			statsHandler(stats).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats).ServeHTTP(w, req)
	})
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	// 发一个请求产生数据
	req, _ := http.NewRequest("POST",
		proxyURL(proxy.URL, backend.URL+"/api"), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-table-test-1234567")
	noCompressClient.Do(req)

	// 拉 table 格式
	resp, err := noCompressClient.Get(proxy.URL + "/__stats?format=table")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("table Content-Type = %q,期望 text/plain", ct)
	}
	s := string(body)
	// 表格应包含列头和分隔线
	if !strings.Contains(s, "COUNT") {
		t.Errorf("表格缺少 COUNT 列头:\n%s", s)
	}
	if !strings.Contains(s, "----") {
		t.Errorf("表格缺少分隔线:\n%s", s)
	}
	// 应包含掩码 key
	masked := maskKey("sk-table-test-1234567")
	if !strings.Contains(s, masked) {
		t.Errorf("表格缺少掩码 key %q:\n%s", masked, s)
	}
}

// 防止编译时未使用的 import 报错(部分场景下 url/time/base64 可能未被引用)
var _ = time.Second
var _ = base64.StdEncoding
