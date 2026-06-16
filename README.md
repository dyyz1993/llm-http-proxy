# llm-http-proxy

一个**百分百透传**的通用反向代理。把完整目标 URL 拼在代理地址后面即可,method / headers(含 Authorization)/ body / query **全部原样转发,不追加任何 header**。

适合在使用 GLM Coding、OpenAI 等 LLM API 时,把请求经本地代理转发出去 —— 客户端无需改动,只在 base URL 前加上代理地址。

## 用法

启动代理:

```bash
go run main.go -addr :8080
```

### 普通 HTTP / SSE

把完整目标 URL 拼在代理路径后:

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

用 OpenAI SDK 时同理,只改 `base_url`:

```python
client = OpenAI(base_url="http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4")
```

### WebSocket

协议头换成 `ws://` / `wss://`,代理自动检测 `Upgrade` 头并切换为双向隧道:

```
ws://localhost:8080/wss://your-ws-endpoint
```

## 透传内容

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
| WebSocket | ✅ 双向隧道 |

## 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8080` | 监听地址 |

## 测试

```bash
go test -v .          # 功能测试(12 个用例,本地自包含)
go test -race .       # 带竞态检测
go test -bench=. -benchmem .   # 性能基准测试
```

## 构建

```bash
go build -o llm-http-proxy .   # 生成单二进制,无运行时依赖
```
