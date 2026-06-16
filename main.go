// glm-proxy: 百分百透传的通用反向代理,支持 WebSocket。
//
// 用法:把完整目标 URL 拼在代理地址后面即可,其余全部原样。
//
//	http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions?code=xxx
//	      └─ 代理 ┘ └──────── 目标完整 URL,原样拼在后面 ────────┘
//
// 透传内容:method / headers(含 Authorization,不追加任何 header)/ body / query / 流式响应。
//
// 兼容类型:
//   - 普通 HTTP:GET/POST/PUT/DELETE/PATCH...
//   - Body:JSON / 纯文本 / 表单 urlencoded / 文件上传 multipart / 二进制
//   - 流式响应:SSE(chunked 边收边发)
//   - WebSocket:检测 Upgrade 头,升级为双向隧道,两个方向同时拷贝数据
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// hostFromURL 从目标 URL 字符串里取 host:port。
// 默认端口:http/ws -> 80,https/wss -> 443。
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "wss://") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// tlsDialWithServerName 建立一条 TLS 连接,SNI 用目标 host。
func tlsDialWithServerName(hostport string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return tls.Dial("tcp", hostport, &tls.Config{ServerName: host})
}

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	flag.Parse()

	handler := newProxyHandler()

	log.Printf("透传代理已启动: http://localhost%s  (支持 HTTP / SSE / WebSocket)", *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatal(err)
	}
}

// newProxyHandler 构造代理的 http.Handler。测试和 main 共用同一份逻辑。
func newProxyHandler() http.Handler {
	transport := &http.Transport{
		DisableCompression: true, // 不偷偷加 Accept-Encoding: gzip
	}
	client := &http.Client{Transport: transport}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw := strings.TrimPrefix(req.RequestURI, "/")
		if raw == "" || !strings.Contains(raw, "://") {
			http.Error(w,
				"请把完整目标 URL 拼在路径里\n", http.StatusBadRequest)
			return
		}

		// WebSocket 分支:检测 Upgrade: websocket
		if isWebSocketUpgrade(req) {
			handleWebSocket(w, req, raw)
			return
		}

		// 普通 HTTP:原样转发
		outReq, err := http.NewRequestWithContext(req.Context(), req.Method, raw, req.Body)
		if err != nil {
			http.Error(w, "目标 URL 无法解析: "+err.Error(), http.StatusBadRequest)
			return
		}
		outReq.Header = req.Header.Clone()    // 原样复制,不追加任何 header
		outReq.ContentLength = req.ContentLength // 显式带上 body 长度,避免 body 不被发送

		resp, err := client.Do(outReq)
		if err != nil {
			http.Error(w, "转发失败: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		dst := w.Header()
		for k, vs := range resp.Header {
			dst[k] = vs // 响应头也原样透传
		}
		w.WriteHeader(resp.StatusCode)

		// 流式转发:支持 SSE,边收边 flush
		if f, ok := w.(http.Flusher); ok {
			buf := make([]byte, 32*1024)
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					f.Flush()
				}
				if rerr != nil {
					break
				}
			}
		} else {
			io.Copy(w, resp.Body)
		}
	})
}

// isWebSocketUpgrade 判断是否为 WebSocket 升级请求。
func isWebSocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// handleWebSocket 把 WebSocket 连接作为原始 TCP 双向隧道转发。
// 不解析 WS 帧协议,直接在底层 TCP 上两个方向同时拷贝字节流 ——
// 这样对 WS 子协议(普通/加密/自定义)全部透明兼容。
func handleWebSocket(w http.ResponseWriter, req *http.Request, targetURL string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "服务器不支持 WebSocket(无法 hijack)", http.StatusInternalServerError)
		return
	}

	// 解析目标 URL,取 host:port 和 path+query。
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" {
		http.Error(w, "目标 URL 无法解析", http.StatusBadRequest)
		return
	}
	hostport := hostFromURL(targetURL)
	useTLS := strings.HasPrefix(targetURL, "wss://") || strings.HasPrefix(targetURL, "https://")

	// 连接上游
	var backend net.Conn
	if useTLS {
		backend, err = tlsDialWithServerName(hostport)
	} else {
		backend, err = net.DialTimeout("tcp", hostport, 30*time.Second)
	}
	if err != nil {
		http.Error(w, "连接上游失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()

	// hijack 到底层 TCP
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// 手动构造握手请求行+头,避免 req.Write 用被规范化的 URL。
	// 请求行的 path 必须用原始 RequestURI 里的(去掉目标 host 前缀后的剩余部分)。
	// targetURL 形如 wss://ws.postman-echo.com/raw
	// 客户端原始 RequestURI 形如 /wss://ws.postman-echo.com/raw
	// 我们需要的 path 是 u.RequestURI()(= "/raw")。
	requestPath := u.RequestURI()

	// 写请求行
	fmt.Fprintf(backend, "%s %s HTTP/1.1\r\n", req.Method, requestPath)
	// 写 Host(用上游真实 host)
	fmt.Fprintf(backend, "Host: %s\r\n", u.Host)
	// 原样转发其余 header(除 Host 外,因为 Host 上面已写)
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(backend, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(backend, "\r\n")

	// 把 hijack bufio 里已缓冲的客户端数据先冲给上游
	if clientBuf != nil {
		if n := clientBuf.Reader.Buffered(); n > 0 {
			io.Copy(backend, clientBuf)
		}
	}

	// 双向隧道:两个方向同时拷贝字节流
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(clientConn, backend)
		if tcp, ok := clientConn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(backend, clientConn)
		if tcp, ok := backend.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	wg.Wait()
}
