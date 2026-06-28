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

## Key 注入模式（可选）

启动时加 `-keys keys.yaml` 启用，用户通过 `/k/{alias}/` 路径访问，真实 API key 只存在服务端。

```bash
# keys.yaml 配置
cat > keys.yaml << 'EOF'
glm:
  key: "你的GLM真实key"
  rate: 60        # 限流:每分钟最多 60 次
  burst: 10       # 突发上限 10

claude:
  key: "你的Claude真实key"
  header: x-api-key
EOF

# 启动
llm-http-proxy -addr :8080 -keys keys.yaml -admin MyPassword

# 使用(用户不需要带 key,服务端自动注入)
curl http://localhost:8080/k/glm/https://open.bigmodel.cn/api/paas/v4/chat/completions \
  -H "Authorization: Bearer anything" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}'
```

### Header 自动检测

不写 `header` 字段时，代理根据路径自动选择注入方式：
- 路径含 `/anthropic/`（如 Claude API）→ `x-api-key`
- 其他 → `Authorization: Bearer`

客户端带什么 header，就自动替换什么 header（支持同时带 `x-api-key` 和 `Authorization`）。

---

## 高级配置（keys.yaml）

在 keys.yaml 中可配置用量限额、禁止时段、费用乘数、图片过滤等高级功能。

### 用量限额（MaxTokens / MaxRequests / Window）

每个 alias 可配置窗口期内的 token 用量和请求次数上限。超限后返回 402 Payment Required。

```yaml
glm:
  key: "你的GLM真实key"
  max_tokens: 10000000    # 窗口期内最多 1 千万 token(输入+输出)
  max_requests: 5000      # 窗口期内最多 5000 次成功请求
  window: 24h             # 窗口期时长: 5h / 24h / 7d / 30d(空=默认100天)
```

窗口自动滚动：窗口过期后计数器自动重置。

### 禁止时段（TimeBlock）

限制某个 alias 每天只能在特定时段使用：

```yaml
glm:
  key: "你的GLM真实key"
  time_block:
    start: "22:00"    # 每天22:00开始禁止
    end: "08:00"      # 次日08:00结束禁止(跨午夜)
```

- `start < end`：单日区间（如 09:00-18:00）
- `start > end`：跨午夜区间（如 22:00-08:00）
- `start == end`：全天禁止
- 被禁时段请求返回 403 Forbidden

### Token 用量乘数（token_multipliers）

某些模型按实际用量×N 计费，可配置乘数规则：

```yaml
token_multipliers:
  - models: ["glm-5*"]           # glob 匹配模型名
    multiply: 3.0                # 用量×3
  - models: ["claude-3-opus*"]
    domains: ["api.anthropic.com"]
    multiply: 2.0
  - domains: ["free-test.example.com"]
    multiply: 0                  # 免费不计费
```

- Models 和 Domains 用 glob 匹配（`*` 通配符），大小写不敏感
- 多条规则同时命中时，乘数相乘叠加
- 作用于 Prompt / Cached / Completion token + 所有费用字段

### 图片过滤（image_filter）

某些模型不支持 image_url 内容块，可自动替换为文本占位符：

```yaml
image_filter:
  - models: ["deepseek-chat"]
    domains: ["api.deepseek.com"]
    action: to_text    # 替换 image_url 为 [Image] 文本
```

- `to_text`：替换为 `[Image]` 文本（推荐）
- `strip`：直接删除 image_url 块
- Models / Domains 为空 = 匹配所有

### 完整示例

```yaml
glm:
  key: "sk-glm-key"
  rate: 60
  burst: 10
  max_tokens: 10000000
  max_requests: 5000
  window: 24h
  time_block:
    start: "22:00"
    end: "08:00"

claude:
  key: "sk-claude-key"
  header: x-api-key

# 全局规则
token_multipliers:
  - models: ["glm-5*"]
    multiply: 3.0

image_filter:
  - models: ["deepseek-chat"]
    action: to_text
```

---

## 管理界面（Web UI）

启动时加 `-admin <密码>` 启用，访问 `http://<代理地址>/__admin`：

| 页面 | 路径 | 功能 |
|------|------|------|
| Dashboard | `/__admin` | Token 用量统计 + 费用汇总 + 限额状态 |
| Keys | `/__admin/keys` | 查看/添加/删除 alias 配置 |
| Stats | `/__admin/stats` | 请求来源统计(同 `/__stats`) |
| Logs | `/__admin/logs` | 最近 200 条请求日志(含 token/费用/乘数) |
| Settings | `/__admin/settings` | 运行时添加/移除域名白名单 |

管理界面支持：
- Token 用量统计表（输入/缓存/输出/费用 + 命中率柱状图）
- 单条日志查看（含 TTFB、╳乘数徽标、流式标识 ⚡）
- 运行时修改域名白名单（无需重启）

---

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8080` | 监听地址 |
| `-persist` | (空) | 统计持久化文件路径。指定后重启不丢统计 |
| `-keys` | (空) | key 注入配置文件路径(keys.yaml)。启用后支持 `/k/{alias}/` 模式 |
| `-admin` | (空) | 管理界面密码。设置后启用 Web 管理 UI，路径 `/__admin` |
| `-allow-domains` | (空) | 域名白名单(逗号分隔)。key 注入模式下限制可代理的域名 |
| `-version` | | 打印版本号 |

示例：
```bash
llm-http-proxy -addr :3000
llm-http-proxy -addr :8080 -persist /var/lib/llm-http-proxy/stats.json
llm-http-proxy -keys /etc/llm-http-proxy/keys.yaml -admin MyPassword -allow-domains open.bigmodel.cn,api.z.ai
```

---

## 统计端点

`GET /__stats` —— 查询请求来源统计（只统计，不泄露隐私）。

## 版本端点

`GET /__version` —— 查询版本号、编译时间、启动时间、运行时长。

```bash
curl http://localhost:8080/__version
```

返回：

```json
{
  "version": "v1.5.0",
  "build_time": "2026-06-17T10:00:00Z",
  "start_time": "2026-06-17T18:30:00+08:00",
  "uptime": "2h15m30s"
}
```

| 字段 | 说明 |
|------|------|
| `version` | 版本号（tag 注入，如 `v1.5.0`；本地 `go run` 为 `dev`） |
| `build_time` | 二进制编译时刻（CI 构建时注入） |
| `start_time` | 当前进程启动时刻 |
| `uptime` | 已运行时长（每次查询实时计算） |

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

## 开发：Git Hooks（本地检查）

仓库提供 pre-commit / pre-push 钩子，本地开发时自动检查代码质量：

- **pre-commit**（提交时）：跑 `go vet` + `gofmt` 检查，快速拦截坏代码
- **pre-push**（推送时）：跑完整 `go test -race`，确保不推坏代码

**安装**（clone 后执行一次）：
```bash
bash scripts/install-hooks.sh
```

**跳过**（紧急情况）：
```bash
git commit --no-verify
git push --no-verify
```

> CI 也会跑同样的检查，hooks 只是让你在本地更早发现问题。

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
