# llm-http-proxy 接入指南

> 经本代理转发你的 API 请求，**客户端代码几乎不用改** —— 只需把请求地址前面加上代理地址。

---

## 代理地址

```
https://p.19930810.xyz:8443/<你的目标完整URL>
```

**唯一规则**：原来你怎么请求目标 API，现在还怎么请求，只是在地址前加上 `https://p.19930810.xyz:8443/`。header、body、method 全部不动。

---

## 使用示例

### 示例 1：curl 命令行

```bash
# 原来(直连 GLM)
curl https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"你好"}]}'

# 现在(经代理)—— 只改地址,其余一字不动
curl https://p.19930810.xyz:8443/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"你好"}]}'
```

### 示例 2：Python（OpenAI SDK）

```python
from openai import OpenAI

# 原来(直连)
client = OpenAI(
    base_url="https://open.bigmodel.cn/api/coding/paas/v4",
    api_key="YOUR_KEY",
)

# 现在(经代理)—— 只改 base_url
client = OpenAI(
    base_url="https://p.19930810.xyz:8443/https://open.bigmodel.cn/api/coding/paas/v4",
    api_key="YOUR_KEY",
)

resp = client.chat.completions.create(
    model="glm-4.6",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

### 示例 3：Node.js

```javascript
import OpenAI from "openai";

const client = new OpenAI({
    // 经代理:在原 baseURL 前加上代理地址
    baseURL: "https://p.19930810.xyz:8443/https://open.bigmodel.cn/api/coding/paas/v4",
    apiKey: "YOUR_KEY",
});
```

### 示例 4：流式输出（SSE）

LLM 最常用的流式场景，加 `-N` 禁用缓冲即可实时收到：

```bash
curl -N https://p.19930810.xyz:8443/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"数1到5"}],"stream":true}'
```

SDK 里设置 `stream=True` 即可，代理对流式完全透明。

---

## 兼容性

| 类型 | 支持 |
|------|------|
| GET / POST / PUT / DELETE / PATCH | ✅ |
| JSON / 表单 / 文件上传 / 二进制 body | ✅ 原样转发 |
| 自定义 header（含 Authorization） | ✅ 原样转发，不追加任何 header |
| 流式响应（SSE） | ✅ 实时转发，不缓冲 |
| WebSocket | ✅ `wss://p.19930810.xyz:8443/wss://目标` |

---

## 🔑 Key 注入模式（无需自带 key）

除了上面的"自带 key 透传"模式，还支持 **key 注入模式**：你不需要带 API key，代理根据路径标识自动注入对应的 key。

### 用法

```
https://p.19930810.xyz:8443/k/{标识}/https://目标URL
```

**示例（GLM）**：
```bash
# 用户不需要带 Authorization!代理自动注入
curl https://p.19930810.xyz:8443/k/glm/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"你好"}]}'
```

**Python SDK**：
```python
client = OpenAI(
    base_url="https://p.19930810.xyz:8443/k/glm/https://open.bigmodel.cn/api/coding/paas/v4",
    api_key="not-needed",  # key 由代理注入,这里随便填
)
```

### 可用的路径标识

联系管理员获取你可用的标识（如 `glm`、`claude`、`azure`）。每个标识对应一个预配置的 key + 目标域名。

### 两种模式对比

| | 自带 key 透传 | key 注入模式 |
|---|---|---|
| 路径 | `/https://目标` | `/k/{标识}/https://目标` |
| 需要带 key | ✅ 是 | ❌ 不需要 |
| key 安全性 | 用户持有 | **服务端持有，用户看不到** |
| 限流 | 无 | 按标识限流（可配） |
| 换 key | 需通知所有用户 | 改配置即可，用户无感 |

### 限流说明

key 注入模式支持按标识限流。超过限额返回 `429 Too Many Requests`，响应头含 `Retry-After`。

---

## 🔒 隐私声明：我们不收集什么

这是开源项目，代码完全公开。我们**明确声明**：

### ❌ 绝对不收集、不记录

| 数据 | 说明 |
|------|------|

## 🔒 隐私声明：我们不收集什么

这是开源项目，代码完全公开。我们**明确声明**：

### ❌ 绝对不收集、不记录

| 数据 | 说明 |
|------|------|
| **明文 API Key** | 永远不记录明文。key 提取后立即掩码（如 `sk-****wxyz`），掩码后无法还原 |
| **请求 body** | 你的 prompt 内容、对话、代码 —— **完全不记录**，原样转发 |
| **完整 URL 路径** | 只记录目标域名（如 `open.bigmodel.cn`），**不记录 path/query** |
| **完整请求头** | 除掩码 key 外，不记录其他 header |
| **响应内容** | 原样转发给你，不记录 |

### ✅ 只记录以下脱敏数据（用于统计）

| 数据 | 处理方式 | 举例 |
|------|---------|------|
| 来源 IP | 直接记录 | `203.0.113.5` |
| API Key | **掩码**（前缀+后4位，中间 `*`） | `sk-abcd1234wxyz` → `sk-****wxyz` |
| 目标域名 | 只记 host | `open.bigmodel.cn` |
| 状态码 | 直接记录 | `200` |
| 调用次数 | 计数 | `42` |

### 🔍 你可以自己验证

代码完全开源，你可以逐行审查：

- **开源地址**：https://github.com/dyyz1993/llm-http-proxy
- **核心文件**：`main.go`（转发逻辑）+ `stats.go`（统计/日志）
- **快速验证**：
  ```bash
  git clone https://github.com/dyyz1993/llm-http-proxy
  cd llm-http-proxy
  # 搜"有没有偷传数据"
  grep -rn "http.Get\|http.Post" *.go | grep -v _test.go
  # 看 key 怎么处理(应有 maskKey 掩码)
  grep -n "maskKey" stats.go
  ```

更详细的安全审查见 [SECURITY-AUDIT.md](SECURITY-AUDIT.md)。

---

## 🛡️ 透明性保证

代理会**自动剥离反代指纹头**（`X-Forwarded-*`、`Via`、`CF-*` 等），让目标 API 收到的请求和你直连时**完全一致**。目标 API 无法从请求特征判断出经过了代理。

---

## 常见问题

**Q: 代理会增加多少延迟？**
A: 几乎为零。代理本身处理时间 < 1ms，实际延迟主要来自目标 API 的响应时间。

**Q: 代理会改我的请求吗？**
A: 不会。method / header / body / query 全部原样转发，唯一做的是剥离反代指纹头（为了让请求更"干净"）。

**Q: 上游 API 挂了怎么办？**
A: 上游返回错误码（500 等）时，错误响应原样转给你；上游完全连不上时，代理返回 502。

**Q: 我的 key 安全吗？**
A: 代理只在内存中原样转发你的 key 给目标 API（这是必须的）。key **不以明文落盘、不进日志** —— 统计里只有掩码后的 key，无法还原。

**Q: 我想自己审查代码，从哪开始？**
A: https://github.com/dyyz1993/llm-http-proxy —— 从 `main.go` 的 `newProxyHandler` 函数开始读，那是转发核心。

---

## 开源信息

- **仓库**：https://github.com/dyyz1993/llm-http-proxy
- **License**：MIT
- **语言**：Go（标准库，无第三方黑盒依赖）
- **问题反馈**：https://github.com/dyyz1993/llm-http-proxy/issues
