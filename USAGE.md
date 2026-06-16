# llm-http-proxy 使用说明

> 本文档是**自包含**的：读完即可掌握全部用法，无需查阅其他文件。
> 也适合直接提供给 AI 助手，让它据此指导部署和使用。

---

## 这是什么

一个**百分百透传**的反向代理。把目标 URL 拼在代理地址后面，请求会被原样转发：

```
http://<代理地址>/<完整目标URL>
```

- method / headers（含 Authorization）/ body / query **全部原样转发，不追加任何 header**
- 支持 HTTP 全方法、SSE 流式、WebSocket
- 自带请求来源统计（IP + 掩码 key + 状态码 + 时间窗口），不泄露隐私

**典型用途**：给 GLM / OpenAI / Claude 等 LLM API 做代理，客户端只需在 base URL 前加上代理地址。

---

## 30 秒上手

```bash
# 1. 启动（任选一种）
docker run -d -p 8080:8080 ghcr.io/dyyz1993/llm-http-proxy:latest
# 或: go run main.go -addr :8080

# 2. 使用 —— 只在目标 URL 前加上代理地址
curl http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}'

# 3. 看统计
curl http://localhost:8080/__stats
```

---

## 部署方式

### 方式 A：Docker（推荐）

```bash
# 拉取预构建镜像（GHCR，多架构 amd64/arm64）
docker run -d \
  --name llm-http-proxy \
  --restart unless-stopped \
  -p 8080:8080 \
  ghcr.io/dyyz1993/llm-http-proxy:latest
```

换端口（宿主机 `3000` → 容器 `8080`）：
```bash
docker run -d --name llm-http-proxy -p 3000:8080 ghcr.io/dyyz1993/llm-http-proxy:latest
```

容器内改监听端口：
```bash
docker run -d --name llm-http-proxy -p 9090:9090 \
  ghcr.io/dyyz1993/llm-http-proxy:latest -addr :9090
```

带统计持久化（重启不丢）：
```bash
docker run -d --name llm-http-proxy -p 8080:8080 \
  -v /opt/proxy-stats:/data \
  ghcr.io/dyyz1993/llm-http-proxy:latest \
  -persist /data/stats.json
```

### 方式 B：Docker Compose

仓库已含 `docker-compose.yml`：
```bash
docker compose up -d        # 启动
docker compose logs -f      # 日志
docker compose down          # 停止
```

### 方式 C：Go 二进制 + systemd（生产推荐）

从 [Releases](https://github.com/dyyz1993/llm-http-proxy/releases) 下载对应平台二进制，解压到 `/usr/local/bin/`：

```bash
curl -fsSL -o /tmp/p.tar.gz \
  https://github.com/dyyz1993/llm-http-proxy/releases/download/v1.4.0/llm-http-proxy-linux-amd64.tar.gz
tar -xzf /tmp/p.tar.gz -C /usr/local/bin
chmod +x /usr/local/bin/llm-http-proxy-linux-amd64
mv /usr/local/bin/llm-http-proxy-linux-amd64 /usr/local/bin/llm-http-proxy
```

systemd 服务（`/etc/systemd/system/llm-http-proxy.service`）：
```ini
[Unit]
Description=LLM HTTP Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/llm-http-proxy -addr :8080 -persist /var/lib/llm-http-proxy/stats.json
Restart=always
RestartSec=3
ReadWritePaths=/var/lib/llm-http-proxy

[Install]
WantedBy=multi-user.target
```

启用：
```bash
mkdir -p /var/lib/llm-http-proxy
systemctl daemon-reload
systemctl enable --now llm-http-proxy
```

### 方式 D：源码

```bash
go run main.go -addr :8080
# 或编译
go build -o llm-http-proxy . && ./llm-http-proxy
```

---

## 使用方法

### 核心：把目标 URL 拼在代理地址后

```
http://<代理地址>/<完整目标URL>
```

**普通 HTTP / SSE：**
```bash
# 直连
curl https://open.bigmodel.cn/api/coding/paas/v4/chat/completions ...

# 经代理(加前缀)
curl http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions ...
```

**WebSocket：** 协议头换成 `ws://`：
```
ws://localhost:8080/wss://your-ws-endpoint
```

### SDK 配置（OpenAI 兼容）

```python
# 直连
client = OpenAI(base_url="https://open.bigmodel.cn/api/coding/paas/v4")
# 经代理
client = OpenAI(base_url="http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4")
```

---

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8080` | 监听地址 |
| `-persist` | (空) | 统计持久化文件路径。指定后重启不丢统计 |
| `-version` | | 打印版本号 |

示例：
```bash
llm-http-proxy -addr :3000
llm-http-proxy -addr :8080 -persist /var/lib/llm-http-proxy/stats.json
```

---

## 统计端点

`GET /__stats` —— 查询请求来源统计（只统计，不泄露隐私）。

### 查询参数

| 参数 | 取值 | 说明 |
|------|------|------|
| `by` | `ip`(默认) / `key` / `window` | 聚合维度 |
| `format` | `json`(默认) / `table` | 输出格式 |
| `top` | N | 只返回调用最多的 N 个 |
| `hours` | 1-24 | 配合 `by=window`，返回最近 N 小时（默认 24） |

### 示例

```bash
# 按 IP(默认)
curl http://localhost:8080/__stats

# 按 key(反向:看 key 触发了哪些 IP)
curl "http://localhost:8080/__stats?by=key"

# 时间窗口(最近 12 小时,带柱状图)
curl "http://localhost:8080/__stats?by=window&hours=12&format=table"

# Top 5
curl "http://localhost:8080/__stats?top=5"

# 表格视图
curl "http://localhost:8080/__stats?format=table"
curl "http://localhost:8080/__stats?by=key&format=table"
```

### JSON 输出字段

每条记录含：

| 字段 | 类型 | 说明 |
|------|------|------|
| `count` | int | 累计调用次数 |
| `first_seen` | time | 首次访问时间 |
| `last_seen` | time | 最后访问时间 |
| `last_status` | int | 最后一次状态码 |
| `last_target` | string | 最后访问的目标 host（只记 host，不记 path） |
| `status_counts` | map | 各状态码计数，如 `{"200":42,"500":3}` |
| `success_rate` | float | 成功率（2xx 占比，0-1） |
| `distinct_keys` | int | 该 IP 用了多少个不同 key |
| `distinct_ips` | int | 该 key 触发了多少个不同 IP |

### 表格示例

```
IP                 KEY                  COUNT STATUS FIRST_SEEN          LAST_SEEN           TARGET
----------------------------------------------------------------------------------------------------------------------
203.0.113.5        sk-****5678              42    200 2026-06-17 01:00   2026-06-17 02:30   open.bigmodel.cn
----------------------------------------------------------------------------------------------------------------------
去重统计(按 IP):1 个不同 IP,共 1 个不同 key,总计调用 42 次
```

时间窗口表格：
```
HOUR            COUNT BAR
--------------------------------------------------
01:00               0
02:00              10 ████████████████████████████
03:00               5 ██████████████
--------------------------------------------------
总计 3 小时,共 15 次调用
```

### 采集与隐私

- **IP**：`X-Forwarded-For`（第一个）→ `X-Real-IP` → `RemoteAddr`
- **key**：从 `Authorization: Bearer` / `x-api-key` / `api-key` 提取（覆盖 OpenAI / Claude / Azure / GLM），掩码后存储
  - 保留前缀（到首个 `-`）+ 后 4 位，中间用 `*` 填充
  - key 过短（≤8 位）全掩码
  - 提取不到记为 `-`
- **不记录**：body / path / query / 完整 header / 明文 key
- **每请求一行日志**（不含 body）：`req ip=... key=sk-****5678 method=POST host=... status=200 dur=156ms`

### 持久化

默认统计在内存，**重启清空**。加 `-persist <path>` 后：
- 启动时从文件读回历史统计
- 后台每 30 秒原子写入（先 `.tmp` 再 rename）
- 服务重启 / 崩溃恢复 / 升级后统计不丢失
- 时间窗口数据不持久化（过期数据重启后清空，合理）

---

## 兼容类型

| 类型 | 支持 |
|------|------|
| GET / POST / PUT / DELETE / PATCH | ✅ |
| JSON / 纯文本 body | ✅ |
| 表单 `application/x-www-form-urlencoded` | ✅ |
| 文件上传 `multipart/form-data` | ✅ |
| 二进制 body（哈希校验无损） | ✅ |
| 自定义 header（含 Authorization） | ✅ 原样透传，零追加 |
| 响应头 | ✅ |
| SSE 流式响应 | ✅ 边收边发 |
| WebSocket | ✅ 双向隧道 |

---

## 运维

### Docker
```bash
docker logs -f llm-http-proxy                        # 日志
docker restart llm-http-proxy                        # 重启
docker pull ghcr.io/dyyz1993/llm-http-proxy:latest   # 升级
  && docker restart llm-http-proxy
```

### systemd
```bash
systemctl status llm-http-proxy                      # 状态
systemctl restart llm-http-proxy                     # 重启
journalctl -u llm-http-proxy -f                      # 实时日志
journalctl -u llm-http-proxy --since "1 hour ago"    # 最近 1 小时
```

### 升级（二进制）
```bash
curl -fsSL -o /tmp/p.tar.gz \
  https://github.com/dyyz1993/llm-http-proxy/releases/latest/download/llm-http-proxy-linux-amd64.tar.gz
tar -xzf /tmp/p.tar.gz -C /usr/local/bin
systemctl restart llm-http-proxy
```

---

## 常见问题

**Q: 代理本身会增加多少延迟？**
A: < 1ms（本地压测 9000+ req/s）。实际延迟主要来自上游 API 响应时间。

**Q: 上游挂了会怎样？**
A: 上游返回错误码（500 等）时，错误响应原样透传；上游完全连不上时，代理返回 502 Bad Gateway。

**Q: 能同时转发多个不同的上游吗？**
A: 能。每个请求的目标 URL 独立，可以一个请求转 GLM、下一个转 OpenAI。

**Q: 统计端点会被外部看到吗？**
A: 会。`/__stats` 默认无鉴权（公网可访问）。如需保护，建议用防火墙限制，或部署在内网。

**Q: 占多少资源？**
A: 内存常驻 ~6MB（空闲），并发时按连接数增长。2核/2G 小机器可承载数十并发 LLM 调用。

---

## 项目地址

- 仓库：https://github.com/dyyz1993/llm-http-proxy
- 镜像：`ghcr.io/dyyz1993/llm-http-proxy`
- Releases：https://github.com/dyyz1993/llm-http-proxy/releases
- License：MIT
