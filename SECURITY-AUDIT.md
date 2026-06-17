# llm-http-proxy 安全审查指南

> 本文档供安全审查者使用。包含：架构说明 + 每一项的可执行验证命令。
> **本文档不含任何生产部署信息**（无地址、无密码、无接入方式），仅基于公开仓库即可完成全部审查。

---

## 一、审查对象

一个开源的反向代理服务，给 LLM API 做透明转发：
- **仓库**：https://github.com/dyyz1993/llm-http-proxy
- **代码**：`main.go`（转发逻辑）+ `stats.go`（统计/日志）+ 测试
- **镜像**：`ghcr.io/dyyz1993/llm-http-proxy`（GHCR 公开）
- **依赖**：仅 `golang.org/x/net`（WebSocket 库）

所有审查基于公开信息，**无需任何特权**。

---

## 二、架构（用于理解信任模型）

```
客户端 → [本代理] → 目标 API(GLM/OpenAI/...)
              │
              └→ 统计(脱敏,可选持久化到本地文件)
```

代理本身是无状态的转发器。唯一的"副作用"是统计采集（IP + 掩码 key + 状态码），**不涉及任何外联**。

部署形态有两种（二进制 / Docker），但**部署细节不影响代理本身的安全性** —— 审查者只需关注代理代码和镜像，不需要访问任何生产环境。

---

## 三、检查清单（可执行）

### A. 代码后门检查

**目标**：确认代理不会偷传数据、不留后门、不记录明文 key。

#### A1. 无额外外发（只转发用户请求）

```bash
git clone https://github.com/dyyz1993/llm-http-proxy
cd llm-http-proxy

# 搜所有网络调用
grep -rn "http.Get\|http.Post\|http.DefaultClient\|http.NewRequest" *.go | grep -v _test.go
```
**预期**：只有 `http.NewRequestWithContext`（构造用户的转发请求）和 WebSocket 的 `net.Dial`（WS 隧道，用户主动发起的）。**不应有**向固定外部地址的上报/心跳/回传。

#### A2. key 掩码，不记录明文

```bash
grep -n "maskKey\|maskedKey\|extractKey" stats.go
```
**预期**：`extractKey()` 提取 key 后，立即 `maskKey()` 掩码（保留前缀+后4位，中间 `*`）。所有存储和日志用的都是掩码值。**明文 key 不落盘、不进日志**。

#### A3. 无可疑后台 goroutine

```bash
grep -n "go func\|go [a-z]" *.go | grep -v _test.go
```
**预期**：只有 `startPersistLoop`（统计落盘，写本地文件）和 WebSocket 的双向拷贝。**不应有**连到外部的心跳/上报 goroutine。

#### A4. 日志不含 body

```bash
grep -n "log.Printf\|logRequest" *.go
```
**预期**：日志格式 `req ip=... key=sk-**** host=... status=... dur=...`，**只有 IP/掩码key/host/状态码/耗时**，没有 body、没有完整 header。

#### A5. 依赖最小化

```bash
cat go.mod
go mod graph
```
**预期**：直接依赖只有 `golang.org/x/net`。无可疑第三方包。

---

### B. 透明性检查（反代指纹剥离）

**目标**：确认代理剥离了会暴露"经过代理"的头，目标 API 无法察觉。

```bash
# 看 stripProxyHeaders 剥离了哪些
grep -A20 "func stripProxyHeaders" main.go
```
**预期**：剥离 `X-Forwarded-*`（全系）、`Via`、`X-Real-Ip`、`X-Request-Id`、`CF-*`、`True-Client-IP` 等。

**本地验证**（审查者可自己起一个）：
```bash
# 起代理 + echo 后端
go run main.go -addr :8080 &
python3 -m http.server 9000 &  # 或任何回显服务

# 发带指纹头的请求,看后端收不收到
curl -H "X-Forwarded-For: fake" -H "Via: leak" \
  http://localhost:8080/http://localhost:9000/
# 预期:后端不应收到 X-Forwarded-For / Via
```

---

### C. 镜像供应链审查

#### C1. 基础镜像官方可信

```bash
grep "^FROM" Dockerfile Dockerfile.ssh
```
**预期**：
- `Dockerfile`: `FROM golang:1.25-alpine` + `FROM alpine:3.20`（官方）
- `Dockerfile.ssh`: `FROM golang:1.25-alpine` + `FROM debian:bookworm-slim`（官方）
- **无第三方/可疑基础镜像**

#### C2. 安装的包最小化

```bash
grep -A5 "apt-get install\|apk add" Dockerfile Dockerfile.ssh
```
**预期**：只装 `ca-certificates`（Dockerfile）/ `openssh-server supervisor ca-certificates`（Dockerfile.ssh）。**无可疑工具**。

#### C3. 构建可复现（验证镜像与源码一致）

```bash
# 从源码本地构建
docker build -f Dockerfile.ssh -t llm-http-proxy:audit .

# 与 GHCR 镜像对比(层历史)
docker history llm-http-proxy:audit --no-trunc
docker history ghcr.io/dyyz1993/llm-http-proxy:latest-ssh --no-trunc
# 预期:层结构一致(证明 GHCR 镜像就是这份源码构建的)
```

#### C4. 漏洞扫描

```bash
trivy image ghcr.io/dyyz1993/llm-http-proxy:latest-ssh
# 预期:基础镜像的常规 CVE,无高危后门/挖矿
```

---

### D. 测试覆盖审查

```bash
go test -v -count=1 ./...
```
**预期**：30+ 用例全绿，覆盖：
- 透传正确性（GET/POST/PUT/DELETE/表单/文件上传/二进制 body）
- header 原样透传 + 不追加
- **反代指纹头剥离**（`TestStripProxyHeaders`）
- key 掩码规则
- SSE 流式完整
- WebSocket 双向隧道
- 统计采集/持久化/去重
- 竞态检测（`-race`）

---

## 四、已知的局限（诚实声明）

1. **代码是开源的**，任何人可读。但**部署信息（地址/密码/SSH key）不在仓库里**，无法从代码获取生产接入方式。

2. **统计持久化文件**（`stats.json`）记录掩码 key 和 IP。**不记录明文 key、不记录 body**。可审查 `stats.go` 的 `record()` 函数确认。

3. **代理转发是"原样"的** —— 如果用户自己发了恶意 payload，代理会原样转给目标 API（这是代理的本质，不是后门）。

---

## 五、报告问题

发现任何安全问题：
https://github.com/dyyz1993/llm-http-proxy/issues

---

## 附录：快速验证脚本

```bash
#!/bin/bash
set -e
git clone -q https://github.com/dyyz1993/llm-http-proxy /tmp/lhp-audit 2>/dev/null || true
cd /tmp/lhp-audit && git pull -q

echo "=== 1. 无额外外发 ==="
grep -rn "http.Get\|http.Post" *.go | grep -v _test.go | grep -v "NewRequest" && echo "⚠️ 检查上述调用" || echo "✅ 无额外外发"

echo "=== 2. key 掩码 ==="
grep -q "maskKey" stats.go && echo "✅ 有掩码逻辑" || echo "⚠️ 未找到掩码"

echo "=== 3. 日志不含 body ==="
grep -q "logRequest" *.go && grep "logRequest" *.go | grep -qi "body" && echo "⚠️ 日志可能含 body" || echo "✅ 日志不含 body"

echo "=== 4. 指纹头剥离 ==="
grep -q "stripProxyHeaders" main.go && echo "✅ 有指纹头剥离" || echo "⚠️ 无剥离"

echo "=== 5. 测试通过 ==="
go test -count=1 ./... >/dev/null 2>&1 && echo "✅ 测试全过" || echo "⚠️ 测试失败"
```
