# llm-http-proxy

一个**百分百透传**的通用反向代理。把完整目标 URL 拼在代理地址后面即可 —— method / headers(含 Authorization)/ body / query **全部原样转发,不追加任何 header**。

适合在使用 GLM Coding、OpenAI、Claude 等 LLM API 时,把请求经本地代理转发出去 —— 客户端无需改动,只在 base URL 前加上代理地址。自带 **IP 来源 + 掩码 key 统计**(不泄露隐私)。

[![CI](https://github.com/dyyz1993/llm-http-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/dyyz1993/llm-http-proxy/actions/workflows/ci.yml)

---

## 快速开始

### 用法

把完整目标 URL 拼在代理路径后,其余全部不动:

```bash
# 原来(直连)
curl https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -d '{"model":"glm-4.6","messages":[...]}'

# 现在(经代理)—— 只在前面加上代理地址
curl http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -d '{"model":"glm-4.6","messages":[...]}'
```

OpenAI / 兼容 SDK 只改 `base_url`:

```python
client = OpenAI(base_url="http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4")
```

### 部署方式(任选其一)

---

#### 方式 A:Docker(推荐,最简单)

拉取预构建多架构镜像(GHCR):

```bash
docker run -d \
  --name llm-http-proxy \
  -p 8080:8080 \
  ghcr.io/dyyz1993/llm-http-proxy:latest
```

**换端口**(把宿主机 `3000` 映射到容器 `8080`):

```bash
docker run -d \
  --name llm-http-proxy \
  -p 3000:8080 \
  ghcr.io/dyyz1993/llm-http-proxy:latest
# 之后用 http://localhost:3000/... 访问
```

容器内改监听端口(让容器也监听 9090,宿主机映射需对应):

```bash
docker run -d \
  --name llm-http-proxy \
  -p 9090:9090 \
  ghcr.io/dyyz1993/llm-http-proxy:latest \
  -addr :9090
```

---

#### 方式 B:Docker Compose

```bash
docker compose up -d        # 启动(后台)
docker compose logs -f      # 看日志(含统计)
docker compose down          # 停止
```

`docker-compose.yml` 已包含在仓库里,默认拉 `ghcr.io/dyyz1993/llm-http-proxy:latest`。
本地构建则把 `image:` 注释掉、取消 `build: .` 的注释。
换端口编辑 `ports` 和(如需)`command` 即可。

---

#### 方式 C:本地 Docker 构建(不依赖 registry)

```bash
docker build -t llm-http-proxy .
docker run -d -p 8080:8080 llm-http-proxy
```

---

#### 方式 D:下载二进制 Release

从 [Releases 页](https://github.com/dyyz1993/llm-http-proxy/releases) 下载对应平台:

| 平台 | 文件 |
|------|------|
| Linux x86_64 | `llm-http-proxy-linux-amd64.tar.gz` |
| Linux ARM64 | `llm-http-proxy-linux-arm64.tar.gz` |
| macOS ARM (Apple Silicon) | `llm-http-proxy-darwin-arm64.tar.gz` |

```bash
tar -xzf llm-http-proxy-darwin-arm64.tar.gz
./llm-http-proxy-darwin-arm64 -addr :8080
```

---

#### 方式 E:源码运行

```bash
go run main.go -addr :8080
# 或编译
go build -o llm-http-proxy . && ./llm-http-proxy
```

---

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8080` | 监听地址 |

示例:`-addr :3000` 监听 3000 端口。

## 统计端点

代理自带请求来源统计(只统计,不泄露隐私)。支持**双向查询**和**表格视图**:

```bash
# 默认:按 IP 聚合(看每个 IP 用了哪些 key)
curl http://localhost:8080/__stats

# 反向:按 key 聚合(看每个 key 触发了哪些 IP)
curl "http://localhost:8080/__stats?by=key"

# ASCII 表格视图(人读友好)
curl "http://localhost:8080/__stats?format=table"
curl "http://localhost:8080/__stats?by=key&format=table"
```

**by=ip**(默认)—— 每个 IP 用了多少个不同 key:

```json
{
  "203.0.113.5": {
    "keys": {"sk-****5678": {"count": 42, ...}},
    "distinct_keys": 1,
    "total_count": 42
  }
}
```

**by=key** —— 每个 key 触发了多少个不同 IP:

```json
{
  "sk-****5678": {
    "ips": {"203.0.113.5": {"count": 42, ...}},
    "distinct_ips": 1,
    "total_count": 42
  }
}
```

**format=table** —— ASCII 表格:

```
IP                 KEY                                           COUNT STATUS LAST_SEEN                 TARGET
----------------------------------------------------------------------------------------------------------------------
14.19.170.64       f8dc*****************************************CGwA      1    200 2026-06-17 01:27:36      open.bigmodel.cn
14.19.170.64       -                                                1    200 2026-06-17 01:25:53      httpbin.org
----------------------------------------------------------------------------------------------------------------------
去重统计(按 IP):2 个不同 IP,共 3 个不同 key,总计调用 5 次
```

**采集与隐私:**
- IP:`X-Forwarded-For` → `X-Real-IP` → `RemoteAddr`
- key 从 `Authorization: Bearer` / `x-api-key` / `api-key` 提取(覆盖 OpenAI / Claude / Azure / GLM),保留前缀 + 后 4 位,中间 `*`,过短全掩码
- **不记录** body / path / query / 完整 header

每请求还会打一行日志(不含 body):
```
req ip=203.0.113.5 key=sk-****5678 method=POST host=open.bigmodel.cn status=200 dur=156ms
```

## 兼容类型

| 类型 | 支持 |
|------|------|
| GET / POST / PUT / DELETE / PATCH | ✅ |
| JSON / 纯文本 body | ✅ |
| 表单 `application/x-www-form-urlencoded` | ✅ |
| 文件上传 `multipart/form-data` | ✅ |
| 二进制 body | ✅ |
| 自定义 header(含 Authorization) | ✅ 原样透传,零追加 |
| 响应头 | ✅ |
| SSE 流式响应 | ✅ 边收边发 |
| WebSocket | ✅ 双向隧道(`ws://localhost:8080/wss://目标`) |

## 测试

```bash
go test -v .          # 功能测试(17 个用例,本地自包含)
go test -race .       # 带竞态检测
go test -bench=. -benchmem .   # 性能基准测试
```

压测参考(Apple M2 Max):

| 场景 | 延迟 |
|------|------|
| 直连基准 | ~90μs/op |
| 经代理(串行) | ~156μs/op |
| 经代理(并发) | ~103μs/op |
| WebSocket 往返 | ~80μs/op |

## License

MIT
