## 修复目标

让 `/g/{group}/` 路由在 SSE 下真流式（tool_call delta 逐 chunk 到客户端），同时**完全保留** group 的换人重试能力。附带修复一个已知 bug：group 路径下 WebSocket 当前返回 500（`bufferedWriter` 未实现 `Hijacker`）。

## 根因回顾（已用 sse_timing_test.go 钉死）

`handleGroupRoute` (main.go:483) 用 `bufferedWriter` 把整个响应缓冲到内存，目的是"看到可换人状态码就丢弃 buffer 换下一个成员"。但 `bufferedWriter.Flush()` 是 no-op（main.go:635），导致 newProxyHandler 内部 SSE 循环的 `f.Flush()` 全被吞，整段 SSE 攒到 `flushTo(w)` 才一次性吐出。

测试实测：5 个间隔 100ms 的 chunk，`/k/` 路由到达时刻 `[0 101 202 303 403]ms`（流式），`/g/` 路由到达时刻 `[0 0 0 0 0]ms`（被 buffer）。

## 关键洞察（来自代码探索）

1. **newProxyHandler 内部的 retry 已经消化了所有可重试上游错误**（默认 429/500/502/503，retry.go:30）。返回到 group 层的 `status` 只剩：成功(<400)、fallback 429、或不可重试的 4xx/5xx。
2. **状态码在 `main.go:1041` (`WriteHeader`) 那一刻就确定了，且此时 body 一字节都没读**（body 读取循环在 1062-1119）。所以 group 完全可以在 WriteHeader 那一刻决定换不换人——根本不需要 buffer body。
3. **`bufferedWriter` 只在 main.go:541 一处使用**，没有任何外部测试依赖，可以安全删除或替换。
4. **WS 在 group 下当前是坏的**：`bufferedWriter` 没实现 `http.Hijacker`，`handleWebSocket` 的 hijack 断言失败返回 500。

## 实现方案

### 1. 新增 `groupWriter` 类型（替换 `bufferedWriter`）

位置：`main.go`，放在原 `bufferedWriter` 定义附近（591-635）。

```go
// groupWriter 是 group 路由专用 ResponseWriter 包装器。
// 它在 WriteHeader 时根据 on_status 列表决定: 命中换人码则拦截响应(不发给客户端,
// 后续 Write 丢弃), group 换下一个成员重试; 未命中则**直接流式透传**给底层 w
// (含 Flush 转发), 保留 SSE 流式。
type groupWriter struct {
    http.ResponseWriter          // 真实 w
    switchStatuses  []int        // on_status 列表 (group.OnStatus)
    status          int          // 捕获的状态码
    headerWritten   bool
    intercepted     bool         // 命中换人码 → 拦截后续写入
}

func (g *groupWriter) WriteHeader(code int) {
    if g.headerWritten {
        return
    }
    g.headerWritten = true
    g.status = code
    // 命中 on_status → 拦截,不发给底层 w
    for _, s := range g.switchStatuses {
        if s == code {
            g.intercepted = true
            return
        }
    }
    // 未命中 → 真正发给客户端
    g.ResponseWriter.WriteHeader(code)
}

func (g *groupWriter) Write(p []byte) (int, error) {
    if g.status == 0 {
        g.status = 200  // 隐式 200
    }
    if g.intercepted {
        return len(p), nil  // 丢弃
    }
    return g.ResponseWriter.Write(p)
}

// Flush 转发给底层 w, 保证 SSE 边收边 flush。
// intercepted 时 no-op(没东西要冲)。
func (g *groupWriter) Flush() {
    if g.intercepted {
        return
    }
    if f, ok := g.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}

// Hijack 转发给底层 w(虽然 group 入口已为 WS 直接 ServeHTTP(w),
// 这里仍保留接口以备万一)。
func (g *groupWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
    if h, ok := g.ResponseWriter.(http.Hijacker); ok {
        return h.Hijack()
    }
    return nil, nil, errors.New("groupWriter: underlying ResponseWriter 不支持 Hijack")
}
```

### 2. 改造 `handleGroupRoute` (main.go:483-587)

**a) 入口加 WebSocket 分支**（紧跟 group/成员解析之后，循环之前）：

```go
// WebSocket 不参与换人(无法 buffer/重放), 直接流式转发到第一个可用成员。
if isWebSocketUpgrade(req) {
    // 选第一个未冷却成员
    for _, member := range cfg.Members {
        st := gm.memberStatus(member)
        if st.IsCooling {
            continue
        }
        memberCfg, ok := ks.lookup(member)
        if !ok {
            continue
        }
        memberCfg = resolveConfig(memberCfg, ks.getInterceptorProfiles(), "default")
        ctx := &CheckContext{ /* 同循环内 */ }
        if !runChecks(w, keyRouteChecks, ctx) {
            gm.markCooldown(member, groupName, /* status from w */)
            continue
        }
        req.RequestURI = "/" + target
        newProxyHandler(stats, ctx.HeadersToInject, ctx.StatLabel,
            ctx.ImageFilter, ctx.TokenMultipliers, ctx.RetryConfig).ServeHTTP(w, req)
        return
    }
    // 全部冷却/拦截 → 503
    http.Error(w, "所有上游成员暂时不可用 (WS)\n", http.StatusServiceUnavailable)
    return
}
```

**b) 把循环里的 `newBufferedWriter()` 换成 `groupWriter`**：

```go
// 原: rec := newBufferedWriter()
rec := &groupWriter{
    ResponseWriter: w,
    switchStatuses: cfg.OnStatus,
}
if !runChecks(rec, keyRouteChecks, ctx) {
    // 拦截器拒绝(402/403/429) → status 已记在 rec.status, body 被丢弃
    gm.markCooldown(member, groupName, rec.status)
    continue
}

req.RequestURI = "/" + target
newProxyHandler(stats, ctx.HeadersToInject, ctx.StatLabel,
    ctx.ImageFilter, ctx.TokenMultipliers, ctx.RetryConfig).ServeHTTP(rec, req)

// newProxyHandler 返回后判定
if rec.intercepted {
    // 命中 on_status → 换人
    gm.markCooldown(member, groupName, rec.status)
    log.Printf("group 成员返回 %d → 换人: group=%s member=%s", rec.status, groupName, member)
    continue
}

// 未命中换人: 响应已直接流式发给客户端
if rec.status >= 200 && rec.status < 400 {
    gm.markSuccess(member, rec.status)
} else {
    // 非 on_status 的 4xx/5xx(如 404/413): 透传给客户端, 同时冷却该成员
    gm.markCooldown(member, groupName, rec.status)
}
return
```

**c) 删除原 main.go:557-580 的 `rec.status >= 200 && rec.status < 400` + `flushTo` 分支**（被新逻辑取代）。

### 3. 删除 `bufferedWriter` 类型 (main.go:589-635)

确认无任何外部引用后删除（grep 已验证仅 main.go:541 一处使用）。`flushTo` / `newBufferedWriter` 一并删除。

### 4. 保留并增强诊断测试 `sse_timing_test.go`

- `TestSSETiming_GRoute` 修复后期望变成 **PASS**（spread ≥ 300ms）。无需改测试代码——它就是回归基线。
- `TestSSETiming_KRoute` 保持不变（已 PASS，作为对照组）。
- 新增一个测试 `TestGroupRoute_E2E_SSEStillSwitchesOnStatus`：验证流式改造**没有**破坏换人能力——成员 1 返回命中 on_status 的 SSE 响应，应该换成员 2，且最终客户端只收到成员 2 的内容。

## 行为变化总结

| 场景 | 改造前 | 改造后 |
|------|--------|--------|
| `/g/` SSE 流式 | ❌ 整坨到达 | ✅ 逐 chunk 流式 |
| `/g/` WebSocket | ❌ 返回 500 | ✅ 正常工作 |
| `/g/` 普通 JSON 响应 | buffer 到内存 | 直接流式（行为一致,只是少占内存） |
| `/g/` 拦截器拒绝(402/403/429) | buffer 后丢弃换人 | 直接丢弃换人（无客户端可见差异） |
| `/g/` 上游 on_status 命中 | buffer 后丢弃换人 | WriteHeader 拦截后丢弃换人（无客户端可见差异） |
| `/g/` 上游 4xx/5xx 非 on_status | flush 给客户端 | 直接透传给客户端（无差异） |
| `/k/` `/`（透传） | 流式 | 不变 |

## 已知限制（不在本次修复范围,需告知用户）

- **on_status 配 502/503 实际不生效**：newProxyHandler 内部 retry 会把上游 502/503 转成 fallback 429（main.go:1008-1014），所以 group 看到的永远是 429，配 502/503 是无效的。这是 pre-existing 问题，本次修复不引入也不解决。可在 AGENTS.md / README 补一句说明。
- **TTFB 在 group 多成员切换时只反映最终成功那一个成员**（pre-existing，不修）。

## 验证步骤

1. `GOROOT=/usr/local/go PATH=/usr/local/go/bin:$PATH go test ./... -short` — 全绿
2. `GOROOT=/usr/local/go PATH=/usr/local/go/bin:$PATH go test -run 'TestSSETiming|TestGroupRoute' -v` — `/g/` timing 测试由 FAIL 转 PASS
3. `/usr/local/go/bin/gofmt -w main.go sse_timing_test.go`
4. 不改 CI、不发版（按你之前的发布流程，由你手动走 release.yml）

## 改动文件清单

- `main.go`：新增 `groupWriter`；改造 `handleGroupRoute`（WS 分支 + groupWriter 替换 + 换人判定逻辑）；删除 `bufferedWriter`/`newBufferedWriter`/`flushTo`
- `sse_timing_test.go`：新增 `TestGroupRoute_E2E_SSEStillSwitchesOnStatus` 验证换人能力未破坏

无配置文件、CI、Docker 改动。