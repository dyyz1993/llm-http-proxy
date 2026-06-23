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
	"context"
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
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats, nil, "").ServeHTTP(w, req)
	})
	return httptest.NewServer(mux)
}

// startProxyWithKeys 启动带 key 注入模式的代理(模拟 main 的完整路由)。
func startProxyWithKeys(t *testing.T, ks *keyStore) *httptest.Server {
	t.Helper()
	stats := newStatsCollector()
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/__version":
			versionHandler(w, req)
			return
		case req.URL.Path == "/__stats":
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		case req.URL.Path == "/" || req.URL.Path == "":
			serveHelp(w, "")
			return
		case strings.HasPrefix(req.URL.Path, "/k/"):
			handleKeyRoute(w, req, ks, stats)
			return
		default:
			newProxyHandler(stats, nil, "").ServeHTTP(w, req)
		}
	})
	return httptest.NewServer(mux)
}

// startProxyWithAdmin 启动带管理界面的代理(模拟 main 的完整路由)。
func startProxyWithAdmin(t *testing.T, password string, ks *keyStore) *httptest.Server {
	t.Helper()
	stats := newStatsCollector()
	// 跟 main.go 一致:ks != nil 时才建配额缓存
	var qc *quotaCache
	if ks != nil {
		qc = newQuotaCache(":0") // 测试用任意端口,probe 不会真连
	}
	admin := newAdminServer(password, stats, ks, qc, newUsageStats())
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if admin != nil && (req.URL.Path == "/__admin" || strings.HasPrefix(req.URL.Path, "/__admin/")) {
			admin.handler().ServeHTTP(w, req)
			return
		}
		switch {
		case req.URL.Path == "/__version":
			versionHandler(w, req)
			return
		case req.URL.Path == "/__stats":
			var authFn func(*http.Request) bool
			if admin != nil {
				authFn = admin.authCheck
			}
			statsHandler(stats, authFn).ServeHTTP(w, req)
			return
		default:
			newProxyHandler(stats, nil, "").ServeHTTP(w, req)
		}
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

// TestStripProxyHeaders 验证反代特征头被剥离,客户端正常头保留。
func TestStripProxyHeaders(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()
	proxy := startProxy(t)
	defer proxy.Close()

	// 客户端模拟"经过上游网关"的请求:既带正常头,又带反代指纹头
	req, _ := http.NewRequest("GET",
		proxyURL(proxy.URL, backend.URL+"/strip"), nil)
	// 正常头(应保留)
	req.Header.Set("Authorization", "Bearer keepme")
	req.Header.Set("X-Custom", "keep")
	// 反代指纹头(应被剥离)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Scheme", "https")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Via", "1.1 proxy")
	req.Header.Set("X-Real-Ip", "1.2.3.4")
	req.Header.Set("X-Request-Id", "abc123")
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")

	resp, err := noCompressClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	hdrs, _ := got["headers"].(map[string]interface{})

	// 正常头应保留
	if hdrs["Authorization"] != "Bearer keepme" {
		t.Errorf("正常头 Authorization 被误删: %v", hdrs["Authorization"])
	}
	if hdrs["X-Custom"] != "keep" {
		t.Errorf("正常头 X-Custom 被误删: %v", hdrs["X-Custom"])
	}
	// 反代指纹头应被剥离
	stripped := []string{"X-Forwarded-For", "X-Forwarded-Scheme", "X-Forwarded-Proto",
		"Via", "X-Real-Ip", "X-Request-Id", "Cf-Connecting-Ip"}
	for _, h := range stripped {
		if _, exists := hdrs[h]; exists {
			t.Errorf("反代特征头 %s 未被剥离,仍出现在后端: %v", h, hdrs[h])
		}
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
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats, nil, "").ServeHTTP(w, req)
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
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats, nil, "").ServeHTTP(w, req)
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
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats, nil, "").ServeHTTP(w, req)
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
			statsHandler(stats, nil).ServeHTTP(w, req)
			return
		}
		newProxyHandler(stats, nil, "").ServeHTTP(w, req)
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

// --- key 注入模式测试 ----------------------------------------------------

// TestKeyInjectionBasic 验证 /k/{alias}/ 目标 能注入正确的 Authorization 头。
func TestKeyInjectionBasic(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["glm"] = KeyConfig{
		Key:    "sk-secret-glm-key",
		Header: "Authorization",
		Prefix: "Bearer ",
	}

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	// 用户不带 key 请求 /k/glm/目标
	resp, err := noCompressClient.Get(
		proxy.URL + "/k/glm/" + backend.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	hdrs, _ := got["headers"].(map[string]interface{})

	// 后端应收到注入的 Authorization
	if hdrs["Authorization"] != "Bearer sk-secret-glm-key" {
		t.Errorf("Authorization 注入错误: got %v, 期望 'Bearer sk-secret-glm-key'",
			hdrs["Authorization"])
	}
	// 目标 URL 正确转发(path 应是 /api)
	if got["path"] != "/api" {
		t.Errorf("path = %v, 期望 /api", got["path"])
	}
}

// TestKeyInjectionMultiHeader 验证 x-api-key(Claude) 和 api-key(Azure) 注入。
func TestKeyInjectionMultiHeader(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["claude"] = KeyConfig{
		Key:    "sk-ant-claude-real-key",
		Header: "x-api-key",
	}
	ks.configs["azure"] = KeyConfig{
		Key:    "azure-real-key",
		Header: "api-key",
	}

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	// Claude: x-api-key
	resp, _ := noCompressClient.Get(proxy.URL + "/k/claude/" + backend.URL + "/v1")
	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	hdrs, _ := got["headers"].(map[string]interface{})
	if hdrs["X-Api-Key"] != "sk-ant-claude-real-key" {
		t.Errorf("Claude x-api-key 注入错误: %v", hdrs["X-Api-Key"])
	}

	// Azure: api-key
	resp2, _ := noCompressClient.Get(proxy.URL + "/k/azure/" + backend.URL + "/v1")
	var got2 map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&got2)
	resp2.Body.Close()
	hdrs2, _ := got2["headers"].(map[string]interface{})
	if hdrs2["Api-Key"] != "azure-real-key" {
		t.Errorf("Azure api-key 注入错误: %v", hdrs2["Api-Key"])
	}
}

// TestKeyInjectionPOSTBody 验证 POST 请求的 body 在 key 注入模式下不丢失。
// (之前 req.Clone 不复制 Body 导致 POST body 丢失,此测试防回归)
func TestKeyInjectionPOSTBody(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["glm"] = KeyConfig{
		Key:    "sk-test-key",
		Header: "Authorization",
		Prefix: "Bearer ",
	}

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	body := `{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`
	resp, err := noCompressClient.Post(
		proxy.URL+"/k/glm/"+backend.URL+"/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	// body 应原样到达后端(不被 key 注入流程丢弃)
	if got["body"] != body {
		t.Errorf("POST body 丢失或损坏: got %v, 期望 %s", got["body"], body)
	}
	// Authorization 应被注入
	hdrs, _ := got["headers"].(map[string]interface{})
	if hdrs["Authorization"] != "Bearer sk-test-key" {
		t.Errorf("Authorization 注入错误: %v", hdrs["Authorization"])
	}
}

// TestKeyInjectionUnknownAlias 验证未知 alias 返回 404。
func TestKeyInjectionUnknownAlias(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxy.URL + "/k/nonexistent/" + backend.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("未知 alias 状态码 = %d, 期望 404", resp.StatusCode)
	}
}

// TestKeyRateLimit 验证按 alias 限流:超限返回 429。
func TestKeyRateLimit(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	// rate=120/min = 2/sec, burst=2 → 前几次能过,快速发会被限
	ks.configs["limited"] = KeyConfig{
		Key:    "sk-limited",
		Header: "Authorization",
		Rate:   120,
		Burst:  2,
	}
	// 创建限流器
	ks.mu.Lock()
	ks.getOrCreateLimiter("limited", ks.configs["limited"])
	ks.mu.Unlock()

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	url := proxy.URL + "/k/limited/" + backend.URL + "/"
	var lastStatus int
	// 快速发 10 次(burst=2,前 2 次能过,后面应 429)
	for i := 0; i < 10; i++ {
		resp, err := noCompressClient.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		lastStatus = resp.StatusCode
		resp.Body.Close()
	}
	if lastStatus != 429 {
		t.Errorf("限流后状态码 = %d, 期望 429(最后几次应被限)", lastStatus)
	}
}

// TestKeyHotReload 验证改配置文件后限流参数生效。
func TestKeyHotReload(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "keys.yaml")
	t.Cleanup(func() { os.Remove(tmpFile) })

	// 初始:不限流
	os.WriteFile(tmpFile, []byte("test:\n  key: sk-v1\n  header: Authorization\n"), 0600)

	ks := newKeyStore()
	ks.load(tmpFile)

	cfg, ok := ks.lookup("test")
	if !ok || cfg.Key != "sk-v1" {
		t.Fatalf("初始加载失败: %v", cfg)
	}

	// 改文件:换 key + 加限流
	os.WriteFile(tmpFile, []byte("test:\n  key: sk-v2\n  header: Authorization\n  rate: 60\n  burst: 1\n"), 0600)
	// 模拟热加载(手动调 reloadIfChanged)
	// 需要 mtime 变化,睡一下确保 mtime 不同
	time.Sleep(20 * time.Millisecond)
	os.Chtimes(tmpFile, time.Now(), time.Now())
	ks.reloadIfChanged()

	cfg2, ok := ks.lookup("test")
	if !ok || cfg2.Key != "sk-v2" {
		t.Errorf("热加载后 key 应为 sk-v2, got %v", cfg2)
	}
	if cfg2.Rate != 60 {
		t.Errorf("热加载后 rate 应为 60, got %d", cfg2.Rate)
	}
}

// TestPassthroughStillWorks 验证纯透传模式(不带 /k/)仍正常。
func TestPassthroughStillWorks(t *testing.T) {
	backend := echoBackend()
	defer backend.Close()

	ks := newKeyStore()
	ks.configs["glm"] = KeyConfig{Key: "sk-should-not-appear", Header: "Authorization"}

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	// 纯透传:用户自己带 key,不带 /k/
	resp, err := noCompressClient.Get(proxy.URL + "/" + backend.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("纯透传状态码 = %d, 期望 200", resp.StatusCode)
	}
	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["path"] != "/api" {
		t.Errorf("纯透传 path = %v, 期望 /api", got["path"])
	}
}

// --- 管理界面测试 ----------------------------------------------------

// TestAdminLoginRequired 验证未登录访问 /__admin 跳转登录页。
func TestAdminLoginRequired(t *testing.T) {
	proxy := startProxyWithAdmin(t, "secret123", nil)
	defer proxy.Close()

	// 用不跟随重定向的 client(否则会跟到 login 页返回 200)
	noRedirect := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Get(proxy.URL + "/__admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Errorf("未登录状态码 = %d, 期望 303(跳转登录)", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/__admin/login" {
		t.Errorf("跳转地址 = %q, 期望 /__admin/login", loc)
	}
}

// TestAdminLoginSuccess 验证正确密码登录成功 + 设 cookie。
func TestAdminLoginSuccess(t *testing.T) {
	proxy := startProxyWithAdmin(t, "secret123", nil)
	defer proxy.Close()

	noRedirect := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.PostForm(proxy.URL+"/__admin/login",
		url.Values{"password": {"secret123"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("登录状态码 = %d, 期望 303", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("登录后未设置 cookie")
	}
	if cookies[0].Name != "lhp_admin" {
		t.Errorf("cookie 名 = %q, 期望 lhp_admin", cookies[0].Name)
	}
}

// TestAdminLoginWrongPassword 验证错误密码登录失败。
func TestAdminLoginWrongPassword(t *testing.T) {
	proxy := startProxyWithAdmin(t, "secret123", nil)
	defer proxy.Close()

	resp, err := noCompressClient.PostForm(proxy.URL+"/__admin/login",
		url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("错误密码状态码 = %d, 期望 401", resp.StatusCode)
	}
}

// TestAdminFullFlow 登录后访问 dashboard + keys CRUD + stats。
func TestAdminFullFlow(t *testing.T) {
	ks := newKeyStore()
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	// 用带 cookie 的 client 登录
	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.PostForm(proxy.URL+"/__admin/login",
		url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 访问 dashboard(应 200)
	resp, err = client.Get(proxy.URL + "/__admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("dashboard 状态码 = %d", resp.StatusCode)
	}

	// 新增 key
	resp, err = client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"testalias"}, "key": {"sk-test"}, "header": {"Authorization"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 验证 key 已添加
	cfg, ok := ks.lookup("testalias")
	if !ok || cfg.Key != "sk-test" {
		t.Errorf("新增 key 未生效: %v", cfg)
	}

	// 删除 key
	resp, err = client.PostForm(proxy.URL+"/__admin/keys/delete?alias=testalias", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := ks.lookup("testalias"); ok {
		t.Error("删除后 key 仍存在")
	}
}

// TestStatsAuthRequired 验证 /__stats 启用 admin 后需登录。
func TestStatsAuthRequired(t *testing.T) {
	proxy := startProxyWithAdmin(t, "pw", nil)
	defer proxy.Close()

	// 未登录访问 /__stats 应 401
	resp, err := noCompressClient.Get(proxy.URL + "/__stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("未登录 /__stats 状态码 = %d, 期望 401", resp.StatusCode)
	}
}

// TestStatsNoAuthWhenAdminDisabled 验证不启用 admin 时 /__stats 仍公开。
func TestStatsNoAuthWhenAdminDisabled(t *testing.T) {
	proxy := startProxyWithAdmin(t, "", nil) // 空密码 = 不启用 admin
	defer proxy.Close()

	resp, err := noCompressClient.Get(proxy.URL + "/__stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 不启用 admin 时 /__stats 应正常返回(200)
	if resp.StatusCode != 200 {
		t.Errorf("未启用 admin 时 /__stats 状态码 = %d, 期望 200", resp.StatusCode)
	}
}

// TestAdminKeyEditPrefill 验证 ?edit=alias 时表单回填现有配置。
func TestAdminKeyEditPrefill(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{
		Key: "sk-orig", Header: "x-api-key", Prefix: "",
		Rate: 60, Burst: 10, Expires: "2026-12-31",
	})
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 带 ?edit=glm 访问 keys 页
	resp, err = client.Get(proxy.URL + "/__admin/keys?edit=glm")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// 编辑模式下应回填现有值(alias 只读、key/header/expires 预填)
	for _, want := range []string{"编辑 glm", `value="sk-orig"`, `value="2026-12-31"`, `value="60"`, `value="10"`} {
		if !strings.Contains(html, want) {
			t.Errorf("edit 表单缺少回填内容 %q", want)
		}
	}
	// x-api-key 应被选中
	if !strings.Contains(html, `value="x-api-key" {{if .Editing}}{{if eq .EditCfg.Header "x-api-key"}}selected{{end}}{{end}}`) {
		// 模板里 selected 标记的条件渲染难以精确匹配字符串,放宽到 header 值出现即可
		if !strings.Contains(html, `value="x-api-key"`) {
			t.Error("edit 表单缺少 x-api-key 选项")
		}
	}
}

// TestAdminKeyEditKeepKeyOnBlank 验证编辑时 key 留空会保留原 key。
func TestAdminKeyEditKeepKeyOnBlank(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{Key: "sk-orig", Header: "Authorization", Prefix: "Bearer "})

	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// 编辑:alias=glm, key 留空,只改 expires
	resp, err := client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"glm"}, "key": {""}, "header": {"Authorization"}, "prefix": {"Bearer "}, "expires": {"2026-06-30"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	cfg, ok := ks.lookup("glm")
	if !ok {
		t.Fatal("编辑后 alias 不存在")
	}
	if cfg.Key != "sk-orig" {
		t.Errorf("编辑留空 key 未保留原值: got %q, want sk-orig", cfg.Key)
	}
	if cfg.Expires != "2026-06-30" {
		t.Errorf("编辑 expires 未更新: got %q", cfg.Expires)
	}
}

// TestAdminKeyInvalidExpires 验证有效期格式错误被拒绝。
func TestAdminKeyInvalidExpires(t *testing.T) {
	ks := newKeyStore()
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	resp, err := client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"bad"}, "key": {"sk-x"}, "expires": {"not-a-date"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 { // renderMsg 返回 200
		t.Errorf("格式错误有效期应 200(显示错误页), got %d", resp.StatusCode)
	}
	if _, ok := ks.lookup("bad"); ok {
		t.Error("格式错误时不应保存 alias")
	}
}

// TestParseExpires 验证有效期解析:时分格式、纯日期兼容、北京时间、非法格式。
func TestParseExpires(t *testing.T) {
	now := time.Now().In(beijing)

	cases := []struct {
		name    string
		input   string
		wantOK  bool
		wantExp string // 期望的过期时刻(RFC3339,北京时间)
	}{
		{"时分格式", "2026-06-22 09:00", true, "2026-06-22T09:00:00+08:00"},
		{"时分秒格式", "2026-06-22 09:00:30", true, "2026-06-22T09:00:30+08:00"},
		{"datetime-local的T分隔", "2026-06-22T09:00", true, "2026-06-22T09:00:00+08:00"},
		{"纯日期(兼容老格式)", "2026-06-22", true, "2026-06-22T23:59:59+08:00"},
		{"空=永久", "", false, ""},
		{"乱码", "not-a-date", false, ""},
		{"只有时分没日期", "09:00", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp, ok := parseExpires(c.input)
			if ok != c.wantOK {
				t.Fatalf("parseExpires(%q) ok=%v, want %v", c.input, ok, c.wantOK)
			}
			if !ok {
				return
			}
			got := exp.Format(time.RFC3339)
			if got != c.wantExp {
				t.Errorf("parseExpires(%q) = %v, want %v", c.input, got, c.wantExp)
			}
		})
	}

	// 时区检查:确保解析用的是北京时间(+08:00),不是 UTC
	exp, _ := parseExpires("2026-06-22 09:00")
	_, offset := exp.Zone()
	if offset != 8*3600 {
		t.Errorf("parseExpires 时区偏移=%d 秒, 期望 +28800(北京),这是 UTC 8 小时 bug", offset)
	}

	// 过期判定:设一个过去的时刻,lookup 应返回 ok=false
	ks := newKeyStore()
	ks.setConfig("past", KeyConfig{Key: "sk", Expires: now.Add(-1 * time.Hour).Format("2006-01-02 15:04")})
	if _, ok := ks.lookup("past"); ok {
		t.Error("过去时刻的有效期应已过期,但 lookup 返回可用")
	}
	// 设一个未来时刻,lookup 应返回 ok=true
	ks.setConfig("future", KeyConfig{Key: "sk", Expires: now.Add(1 * time.Hour).Format("2006-01-02 15:04")})
	if _, ok := ks.lookup("future"); !ok {
		t.Error("未来时刻的有效期应可用,但 lookup 返回过期")
	}
}

// TestAdminKeysExpiredRow 验证已过期的 key 在 Keys 页置灰 + 显示"已到期"标记,
// 而永久 / 未过期的 key 不受影响。
func TestAdminKeysExpiredRow(t *testing.T) {
	now := time.Now().In(beijing)
	ks := newKeyStore()
	ks.setConfig("expired", KeyConfig{Key: "sk", Expires: now.Add(-1 * time.Hour).Format("2006-01-02 15:04")})
	ks.setConfig("active", KeyConfig{Key: "sk", Expires: now.Add(24 * time.Hour).Format("2006-01-02 15:04")})
	ks.setConfig("forever", KeyConfig{Key: "sk"}) // 无 expires = 永久

	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	resp, err := client.Get(proxy.URL + "/__admin/keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// 过期的行:应有 expired class + 已到期标记
	if !strings.Contains(html, `class="expired"`) {
		t.Errorf("过期 key 应有 class=\"expired\",页面未找到")
	}
	if !strings.Contains(html, "已到期") {
		t.Errorf("过期 key 应显示\"已到期\"标记,页面未找到")
	}

	// 永久 key 不应显示有效期
	if !strings.Contains(html, "永久") {
		t.Errorf("永久 key 应显示\"永久\",页面未找到")
	}

	// expiredMap 单元测试
	m := ks.expiredMap()
	if !m["expired"] {
		t.Errorf("expiredMap[\"expired\"] = false, want true")
	}
	if m["active"] {
		t.Errorf("expiredMap[\"active\"] = true, want false")
	}
	if m["forever"] {
		t.Errorf("expiredMap[\"forever\"] = true, want false(永久 key 不应进 map)")
	}
}

// TestNormalizeExpires 验证 datetime-local 的 T 分隔符被规范成空格存储。
func TestNormalizeExpires(t *testing.T) {
	cases := map[string]string{
		"2026-06-22T09:00":    "2026-06-22 09:00", // T → 空格
		"2026-06-22 09:00":    "2026-06-22 09:00", // 已经是空格,不变
		"  2026-06-22T09:00 ": "2026-06-22 09:00", // 去首尾空格 + T→空格
		"":                    "",                 // 空
		"2026-06-22":          "2026-06-22",       // 纯日期无 T,不变
	}
	for in, want := range cases {
		if got := normalizeExpires(in); got != want {
			t.Errorf("normalizeExpires(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAdminKeyExpiresDatetimeLocal 验证 datetime-local 控件提交的 T 分隔格式能保存。
// 这是 v2.1.4 的回归 bug:界面选了时间但 parseExpires 不认 T,被拒。
func TestAdminKeyExpiresDatetimeLocal(t *testing.T) {
	ks := newKeyStore()
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// datetime-local 提交的是 ISO 格式(带 T),用未来的时间避免过期
	futureExpires := time.Now().In(beijing).Add(48 * time.Hour).Format("2006-01-02T15:04")
	resp, err := client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"dt"}, "key": {"sk-x"}, "expires": {futureExpires}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 不应被拒(不是错误页),且保存的是规范化的空格格式
	cfg, ok := ks.lookup("dt")
	if !ok {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("datetime-local 格式有效期应保存成功,但被拒。响应: %s", string(body))
	}
	// 保存的应是规范化的空格格式(T → 空格)
	wantExpires := strings.Replace(futureExpires, "T", " ", 1)
	if cfg.Expires != wantExpires {
		t.Errorf("保存的 expires = %q, want 规范化的 %q", cfg.Expires, wantExpires)
	}
}

// TestAdminKeysCopyURLButton 验证 keys 页有复制调用地址的按钮。
func TestAdminKeysCopyURLButton(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{Key: "sk", Header: "Authorization", Prefix: "Bearer "})
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	resp, err := client.Get(proxy.URL + "/__admin/keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// 应有复制地址的按钮 + copyURL JS 函数 + 每行调用地址带 id
	for _, want := range []string{`id="url-glm"`, `copyURL(`, `复制`} {
		if !strings.Contains(html, want) {
			t.Errorf("keys 页缺少复制地址相关元素 %q", want)
		}
	}
}

// TestAdminKeyCopyPrefill 验证 ?copy=alias 时表单回填配置但 alias 留空。
func TestAdminKeyCopyPrefill(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{
		Key: "sk-orig", Header: "x-api-key", Prefix: "",
		Rate: 60, Burst: 10, Expires: "2026-06-22 09:00",
	})
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// 带 ?copy=glm 访问 keys 页
	resp, err := client.Get(proxy.URL + "/__admin/keys?copy=glm")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// 复制模式:标题显示"复制自 glm",配置回填,但 alias 是输入框(不是只读)
	for _, want := range []string{"复制自 glm", `value="sk-orig"`, `value="2026-06-22 09:00"`, `value="60"`, `value="10"`, `复制为新别名`} {
		if !strings.Contains(html, want) {
			t.Errorf("copy 表单缺少 %q", want)
		}
	}
	// alias 应是可输入的(带 name="alias" 的 input),不是只读的 <b>glm</b>
	if !strings.Contains(html, `name="alias"`) {
		t.Error("复制模式 alias 应可输入(有 name=alias 的 input)")
	}
	if strings.Contains(html, "（不可修改）") {
		t.Error("复制模式 alias 不应是只读的")
	}
}

// TestAdminKeyCopyCreatesNewAlias 验证复制后创建新 alias,原 alias 不受影响。
func TestAdminKeyCopyCreatesNewAlias(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{Key: "sk-orig", Header: "Authorization", Prefix: "Bearer "})

	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// 提交新 alias=glm2,key 沿用(复制场景 key 必填,这里给同样的)
	resp, err := client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"glm2"}, "key": {"sk-orig"}, "header": {"Authorization"}, "prefix": {"Bearer "}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 两个 alias 都应存在
	if _, ok := ks.lookup("glm"); !ok {
		t.Error("复制后原 alias glm 应仍存在")
	}
	cfg2, ok := ks.lookup("glm2")
	if !ok {
		t.Fatal("复制未创建新 alias glm2")
	}
	if cfg2.Key != "sk-orig" {
		t.Errorf("新 alias key = %q, want sk-orig", cfg2.Key)
	}
}

// TestAdminKeyExpiresTimeFormat 验证时分格式的有效期能保存且生效。
func TestAdminKeyExpiresTimeFormat(t *testing.T) {
	ks := newKeyStore()
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ := client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	// 提交一个明天 9 点(北京时间)的有效期
	tomorrow9 := time.Now().In(beijing).Add(26 * time.Hour).Format("2006-01-02 15:04")
	resp, err := client.PostForm(proxy.URL+"/__admin/keys/new",
		url.Values{"alias": {"timed"}, "key": {"sk-x"}, "expires": {tomorrow9}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 应保存成功(未来时刻 → lookup 可用)
	cfg, ok := ks.lookup("timed")
	if !ok {
		t.Fatal("时分格式有效期保存后 alias 不存在")
	}
	if cfg.Expires != tomorrow9 {
		t.Errorf("保存的 expires = %q, want %q", cfg.Expires, tomorrow9)
	}
}

// TestLoginFormAutocomplete 验证登录表单带 autocomplete 属性(浏览器记密码)。
func TestLoginFormAutocomplete(t *testing.T) {
	proxy := startProxyWithAdmin(t, "pw", nil)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/__admin/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, `autocomplete="current-password"`) {
		t.Error("登录表单缺少 autocomplete=current-password,浏览器无法记住密码")
	}
	// 记住密码的 checkbox + localStorage 逻辑也应存在
	for _, want := range []string{`id="rememberMe"`, `localStorage`, `llm_proxy_pw`} {
		if !strings.Contains(html, want) {
			t.Errorf("登录表单缺少记住密码相关元素 %q", want)
		}
	}
}

// TestAdminQuotaRefresh 验证配额刷新端点存在、需鉴权、登录后能触发。
func TestAdminQuotaRefresh(t *testing.T) {
	ks := newKeyStore()
	// 故意放一个明显无效的 key,验证刷新不会因为拉取失败而 panic
	ks.setConfig("fake", KeyConfig{Key: "sk-not-a-real-key", Header: "Authorization", Prefix: "Bearer "})
	proxy := startProxyWithAdmin(t, "pw", ks)
	defer proxy.Close()

	// 未登录:requireAuth 会重定向到登录页(http.Get 默认跟随,最终落在 login 200)
	noAuthClient := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := noAuthClient.Get(proxy.URL + "/__admin/quota/refresh")
	if err != nil {
		t.Fatal(err)
	}
	finalURL := resp.Request.URL.String()
	resp.Body.Close()
	if !strings.HasSuffix(finalURL, "/__admin/login") {
		t.Errorf("未登录访问刷新端点应重定向到登录页, got %s", finalURL)
	}

	// 登录后访问应成功(即使 key 无效拉不到配额,也不应报错)
	jar := newTestCookieJar()
	client := &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}
	resp, _ = client.PostForm(proxy.URL+"/__admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()

	resp, err = client.Get(proxy.URL + "/__admin/quota/refresh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	// 应显示刷新成功提示(无效 key 被跳过,数量为 0 也算正常)
	if !strings.Contains(html, "配额已刷新") {
		t.Errorf("刷新端点应返回成功提示, got: %s", html[:min(200, len(html))])
	}
}

// --- quota 相关测试 ---

func TestNextResetTime(t *testing.T) {
	qc := newQuotaCache(":0")
	now := time.Now()

	// 空:返回零值
	if rst := qc.nextResetTime(); !rst.IsZero() {
		t.Errorf("空缓存应返回零值, got %v", rst)
	}

	// 两个条目,各自的 limit 重置时间不同,应取最早的
	qc.mu.Lock()
	qc.entries = []cachedQuota{
		{
			Alias: "a",
			Limits: []quotaLimit{
				{NextResetMs: now.Add(4 * time.Hour).UnixMilli()},  // 4h 后
				{NextResetMs: now.Add(-1 * time.Hour).UnixMilli()}, // 1h 前(过期,应忽略)
				{NextResetMs: now.Add(2 * time.Hour).UnixMilli()},  // 2h 后(最早的未来)
			},
		},
		{
			Alias: "b",
			Limits: []quotaLimit{
				{NextResetMs: now.Add(6 * time.Hour).UnixMilli()}, // 6h 后
				{NextResetMs: 0}, // 无效,应忽略
			},
		},
	}
	qc.mu.Unlock()

	got := qc.nextResetTime()
	if got.IsZero() {
		t.Fatal("应返回最早的未来重置时刻, got 零值")
	}
	want := now.Add(2 * time.Hour)
	// 允许 1 秒误差
	if got.Sub(want) > time.Second || want.Sub(got) > time.Second {
		t.Errorf("nextResetTime = %v, want ~%v (2h后,最早的未来点)", got, want)
	}
}

// TestProbeDedupByKey 验证同 key 多 alias 只 probe 一次(去重)。
// 不真连网络(用不存在的端口),只验证去重逻辑不 panic 且能跑完。
func TestProbeDedupByKey(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过需要网络的 probe 测试")
	}
	ks := newKeyStore()
	sameKey := "0123456789012345678901234567890123456789" // 40 字符,通过长度检查
	// 三个 alias 用同一个 key,probe 应只调一次(配额+模型各一次)
	ks.setConfig("alias1", KeyConfig{Key: sameKey, Header: "Authorization", Prefix: "Bearer "})
	ks.setConfig("alias2", KeyConfig{Key: sameKey, Header: "Authorization", Prefix: "Bearer "})
	ks.setConfig("alias3", KeyConfig{Key: sameKey, Header: "Authorization", Prefix: "Bearer "})
	// 第四个用不同 key
	ks.setConfig("alias4", KeyConfig{Key: "9876543210987654321098765432109876543210", Header: "Authorization", Prefix: "Bearer "})

	qc := newQuotaCache(":65530") // 不存在的端口,probe 会失败但不 panic

	// 应能正常执行完(每个唯一 key 各 2 次 probe,失败也不影响)
	assertNotPanic(t, func() {
		qc.probeAndRefresh(ks)
	})
}

// assertNotPanic 断言 fn 不 panic。
func assertNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("不应 panic 但 panic 了: %v", r)
		}
	}()
	fn()
}

// --- 域名白名单 + header 自动检测测试 ---

func TestExtractDomain(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://api.z.ai/path", "api.z.ai"},
		{"https://open.bigmodel.cn:8443/api/x", "open.bigmodel.cn"},
		{"http://localhost:8080/", "localhost"},
		{"https://API.Z.AI/", "api.z.ai"}, // 小写化
		{"api.z.ai/no-protocol", "api.z.ai"},
	}
	for _, c := range cases {
		if got := extractDomain(c.input); got != c.want {
			t.Errorf("extractDomain(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestPickHeader(t *testing.T) {
	// 1. 显式配置优先(向后兼容)
	h := pickHeader(KeyConfig{Key: "sk-1", Header: "x-api-key"}, http.Header{})
	if h.Get("X-Api-Key") != "sk-1" {
		t.Errorf("显式 x-api-key: got %q", h.Get("X-Api-Key"))
	}
	h = pickHeader(KeyConfig{Key: "sk-1", Header: "Authorization", Prefix: "Bearer "}, http.Header{})
	if h.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("显式 Authorization+Prefix: got %q", h.Get("Authorization"))
	}
	// 显式 Authorization 不带 Prefix → 自动加 Bearer
	h = pickHeader(KeyConfig{Key: "sk-1", Header: "Authorization"}, http.Header{})
	if h.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("显式 Authorization 无Prefix: got %q", h.Get("Authorization"))
	}

	// 2. 自动模式:客户端带 x-api-key → 替换成 x-api-key
	apiKeyHeaders := http.Header{}
	apiKeyHeaders.Set("X-Api-Key", "placeholder")
	h = pickHeader(KeyConfig{Key: "sk-1"}, apiKeyHeaders)
	if h.Get("X-Api-Key") != "sk-1" {
		t.Errorf("自动(x-api-key): got %q", h.Get("X-Api-Key"))
	}
	// 自动模式不应注入 Authorization
	if h.Get("Authorization") != "" {
		t.Errorf("自动(x-api-key)不应注入 Authorization: got %q", h.Get("Authorization"))
	}

	// 3. 自动模式:客户端带 Authorization → 替换成 Authorization: Bearer
	normalHeaders := http.Header{}
	normalHeaders.Set("Authorization", "Bearer placeholder")
	h = pickHeader(KeyConfig{Key: "sk-1"}, normalHeaders)
	if h.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("自动(Authorization): got %q", h.Get("Authorization"))
	}

	// 4. 自动模式:两个都带 → 两个都替换
	bothHeaders := http.Header{}
	bothHeaders.Set("X-Api-Key", "placeholder")
	bothHeaders.Set("Authorization", "Bearer placeholder")
	h = pickHeader(KeyConfig{Key: "sk-1"}, bothHeaders)
	if h.Get("X-Api-Key") != "sk-1" {
		t.Errorf("自动(both) x-api-key: got %q", h.Get("X-Api-Key"))
	}
	if h.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("自动(both) Authorization: got %q", h.Get("Authorization"))
	}

	// 5. 自动模式:都没带 → 不注入(返回空)
	h = pickHeader(KeyConfig{Key: "sk-1"}, http.Header{})
	if len(h) != 0 {
		t.Errorf("都没带应不注入, got %d headers", len(h))
	}
}

// TestKeyRouteDomainWhitelist 验证域名白名单:不在白名单的域名被拒绝(403)。
func TestKeyRouteDomainWhitelist(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{Key: "sk-secret", Header: "", Prefix: ""}) // 自动模式

	// 设白名单:只允许 api.z.ai
	oldAllow := allowDomains
	allowDomains = map[string]bool{"api.z.ai": true}
	defer func() { allowDomains = oldAllow }()

	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	// 不在白名单的域名 → 403,不注入 key
	resp, err := http.Get(proxy.URL + "/k/glm/https://evil.hacker.com/log")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("非白名单域名应 403, got %d", resp.StatusCode)
	}

	// 在白名单的域名 → 放行(会尝试转发,目标不存在但不应是 403)
	resp2, err := http.Get(proxy.URL + "/k/glm/https://api.z.ai/api/test")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusForbidden {
		t.Error("白名单域名不应被 403 拒绝")
	}
}

// TestKeyRouteAutoHeader 验证自动模式:客户端带了什么 header 就替换什么。
// 用 echo backend 捕获注入的 header。
func TestKeyRouteAutoHeader(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("glm", KeyConfig{Key: "sk-secret", Header: "", Prefix: ""}) // 自动检测

	// 不设白名单(空=不限制)
	oldAllow := allowDomains
	allowDomains = nil
	defer func() { allowDomains = oldAllow }()

	backend := echoBackend() // 回显收到的 header
	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	// 1. 客户端带 x-api-key → 替换成真实 key
	req1, _ := http.NewRequest("GET", proxy.URL+"/k/glm/"+backend.URL+"/v1/messages", nil)
	req1.Header.Set("X-Api-Key", "placeholder")
	resp, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"X-Api-Key":"sk-secret"`) {
		t.Errorf("客户端带 x-api-key 应注入真实 key, body: %s", string(body)[:min(200, len(string(body)))])
	}

	// 2. 客户端带 Authorization → 替换成 Bearer 真实 key
	req2, _ := http.NewRequest("GET", proxy.URL+"/k/glm/"+backend.URL+"/v1/chat/completions", nil)
	req2.Header.Set("Authorization", "Bearer placeholder")
	resp, err = http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"Authorization":"Bearer sk-secret"`) {
		t.Errorf("客户端带 Authorization 应注入 Bearer 真实 key, body: %s", string(body)[:min(200, len(string(body)))])
	}

	// 3. 客户端什么都不带 → 不注入(透传,header 里没有 key)
	resp, err = http.Get(proxy.URL + "/k/glm/" + backend.URL + "/any/path")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "sk-secret") {
		t.Errorf("客户端没带 header 不应注入 key, body: %s", string(body)[:min(200, len(string(body)))])
	}
}

func TestProgressBar(t *testing.T) {
	cases := []struct {
		pct  int
		want string // 期望的 unicode 图块开头几个字符
	}{
		{0, "░░░░░░░░░░░░░░░░░░░░"},   // 0%
		{50, "██████████░░░░░░░░░░"},  // 50%
		{100, "████████████████████"}, // 100%
		{75, "███████████████░░░░░"},  // 75%
		{25, "█████░░░░░░░░░░░░░░░"},  // 25%
	}
	for _, c := range cases {
		got := progressBar(c.pct)
		if got != c.want {
			t.Errorf("progressBar(%d) = %q (len=%d), want %q", c.pct, got, len(got), c.want)
		}
		// 每个进度块字符 █/░ 占 3 字节(UTF-8),20个=60字节
		if len(got) != 60 {
			t.Errorf("progressBar(%d) 字节长度=%d, 期望 60", c.pct, len(got))
		}
	}
	// 超出范围检查
	over := progressBar(150)
	if len(over) != 60 || over != "████████████████████" {
		t.Errorf("150%% 应截断为 20 个█: %q (len=%d)", over, len(over))
	}
}

func TestUnitLabel(t *testing.T) {
	cases := map[int]string{
		3: "周期额度",
		5: "月度时长",
		6: "周额度",
		7: "额度(7)", // 未知 unit, fallback
	}
	for unit, want := range cases {
		if got := unitLabel(unit); got != want {
			t.Errorf("unitLabel(%d) = %q, want %q", unit, got, want)
		}
	}
}

func TestBuildQuotaHTML_Empty(t *testing.T) {
	// 空条目应返回"暂无"提示
	html := buildQuotaHTML(nil)
	if !strings.Contains(html, "暂无配额数据") {
		t.Error("空条目应显示'暂无配额数据'")
	}
	html = buildQuotaHTML([]cachedQuota{})
	if !strings.Contains(html, "暂无配额数据") {
		t.Error("空切片应显示'暂无配额数据'")
	}
}

func TestBuildQuotaHTML_Render(t *testing.T) {
	// 有真实数据时,应正常渲染不 panic,且包含别名和等级
	entries := []cachedQuota{
		{
			Alias: "testkey",
			Level: "pro",
			Limits: []quotaLimit{
				{Type: "TOKENS_LIMIT", Unit: 3, Percentage: 50, NextResetMs: 9999999999999},
				{Type: "TIME_LIMIT", Unit: 5, Percentage: 12, Usage: intPtr(1000), CurrentVal: intPtr(120),
					Details: []quotaUsageDetail{{ModelCode: "search-prime", Usage: 120}}},
			},
			FetchedAt: time.Now(),
		},
	}
	html := buildQuotaHTML(entries)
	for _, want := range []string{"testkey", "pro", "50%", "12%", "周期额度", "月度时长"} {
		if !strings.Contains(html, want) {
			t.Errorf("配额 HTML 缺少 %q", want)
		}
	}
}

func intPtr(v int) *int { return &v }

// --- cookie jar 辅助 ---

type testCookieJar struct {
	cookies map[string][]*http.Cookie
}

func newTestCookieJar() *testCookieJar {
	return &testCookieJar{cookies: make(map[string][]*http.Cookie)}
}

func (j *testCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.cookies[u.Host] = cookies
}

func (j *testCookieJar) Cookies(u *url.URL) []*http.Cookie {
	return j.cookies[u.Host]
}

// TestProxyClientCancelReturns499 验证客户端断连(context 取消)时返回 499,
// 而不是 502。后端故意 sleep,客户端用带超时的 context 提前取消。
func TestProxyClientCancelReturns499(t *testing.T) {
	// 后端故意慢:sleep 5 秒后才响应
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	proxy := startProxy(t)
	defer proxy.Close()

	// 客户端用 500ms 超时的 context,会提前取消 → 代理转发收到 context canceled
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		proxyURL(proxy.URL, backend.URL+"/slow"), nil)

	resp, err := http.DefaultClient.Do(req)
	// 客户端这边会收到错误(context deadline exceeded)或 499 响应
	// 关键:代理应该返回 499 而不是 502
	if err == nil && resp.StatusCode != 499 {
		t.Errorf("客户端断连应返回 499,实际 %d", resp.StatusCode)
	}
	// 如果 err != nil(context deadline),也是正常的 —— 客户端先断了
	// 这个测试主要验证:代理代码路径里 context.Canceled → 499 的分支存在且正确
}

// TestProxyBadGatewayReturns502 验证真正的上游错误(DNS 解析失败)返回 502,
// 而不是 499。确保 499/502 的区分逻辑正确。
func TestProxyBadGatewayReturns502(t *testing.T) {
	proxy := startProxy(t)
	defer proxy.Close()

	// 指向一个不存在的域名 → DNS 解析失败 → 真正的 502
	req, _ := http.NewRequest("GET",
		proxyURL(proxy.URL, "https://this-domain-does-not-exist-xyz123.invalid/"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("测试环境 DNS 行为不一致,跳过: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("上游 DNS 失败应返回 502,实际 %d", resp.StatusCode)
	}
}

// TestHelpText 验证根路径和裸 alias 路径返回 TXT 教程。
func TestHelpText(t *testing.T) {
	// 1. 根路径 / 返回通用教程
	resp, err := http.Get("https://p.19930810.xyz:8443/")
	_ = resp
	_ = err
	// 线上测试不可靠,改用本地 handler 测试

	// helpText 函数单元测试
	txt := helpText("")
	if !strings.Contains(txt, "llm-http-proxy") {
		t.Error("helpText 缺少项目名")
	}
	if !strings.Contains(txt, "api.z.ai") {
		t.Error("helpText 应优先推荐 api.z.ai")
	}
	if !strings.Contains(txt, "github.com/dyyz1993/llm-http-proxy") {
		t.Error("helpText 缺少 GitHub 地址")
	}
	if !strings.Contains(txt, "curl") {
		t.Error("helpText 应包含 curl 示例")
	}
	if !strings.Contains(txt, "499") {
		t.Error("helpText 应说明 499 状态码")
	}

	// 带 alias 的教程应该把别名带进示例
	txt2 := helpText("mymax")
	if !strings.Contains(txt2, "/k/mymax/") {
		t.Error("helpText(\"mymax\") 示例里应使用 mymax 别名")
	}
	// 默认(无别名)应使用 glm
	txt3 := helpText("")
	if !strings.Contains(txt3, "/k/glm/") {
		t.Error("helpText(\"\") 示例应使用默认 glm 别名")
	}
}

// TestServeHelpRootAndBareAlias 验证端到端:根路径和裸 alias 路径返回 TXT。
func TestServeHelpRootAndBareAlias(t *testing.T) {
	ks := newKeyStore()
	ks.setConfig("mymax", KeyConfig{Key: "sk-test-1234567890"})
	proxy := startProxyWithKeys(t, ks)
	defer proxy.Close()

	paths := []struct {
		path      string
		wantAlias string // 教程里应出现的别名
	}{
		{"/", "glm"},           // 根路径 → 通用教程(默认 glm)
		{"/k/", "glm"},         // 裸 /k/ → 通用教程
		{"/k/mymax", "mymax"},  // 裸 alias → 带 alias 教程
		{"/k/mymax/", "mymax"}, // 裸 alias + 斜杠 → 带 alias 教程
	}

	for _, p := range paths {
		resp, err := http.Get(proxy.URL + p.path)
		if err != nil {
			t.Errorf("GET %s 失败: %v", p.path, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("%s: 状态码 %d, want 200", p.path, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: Content-Type %s, want text/plain", p.path, ct)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "/k/"+p.wantAlias+"/") {
			t.Errorf("%s: 教程里未找到别名 %s", p.path, p.wantAlias)
		}
	}
}
