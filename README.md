# llm-http-proxy

> **百分百透传的反向代理**：把目标 URL 拼在代理地址后，请求原样转发。
> 适合给 GLM / OpenAI / Claude 等 LLM API 做代理。自带来源统计（不泄露隐私）。

[![CI](https://github.com/dyyz1993/llm-http-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/dyyz1993/llm-http-proxy/actions/workflows/ci.yml)
[![Release](https://github.com/dyyz1993/llm-http-proxy/actions/workflows/release.yml/badge.svg)](https://github.com/dyyz1993/llm-http-proxy/releases)

---

## 📖 完整文档

**👉 [USAGE.md](USAGE.md) —— 自包含的完整使用说明（适合人和 AI 阅读）**

涵盖：部署方式（Docker / Compose / 二进制 / 源码）、所有命令行参数、统计端点的全部查询方式、隐私说明、运维命令、FAQ。

> **给 AI 助手用**：把 [USAGE.md](USAGE.md) 的内容直接提供给大模型，它就能据此指导部署和使用。

---

## ⚡ 速查（复制即用）

```bash
# 1. 启动代理
docker run -d -p 8080:8080 ghcr.io/dyyz1993/llm-http-proxy:latest

# 2. 使用：在目标 URL 前加上代理地址，其余不动
curl http://localhost:8080/https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" \
  -d '{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}'

# 3. 看统计（IP + 掩码 key + 成功率 + 时间窗口）
curl http://localhost:8080/__stats
curl "http://localhost:8080/__stats?by=window&hours=12&format=table"
```

**一句话用法**：原来怎么请求，现在还怎么请求，只是在地址前加上 `http://<代理地址>/`。

---

## 特性

| 能力 | 说明 |
|------|------|
| 百分百透传 | method / headers / body / query 全部原样转发，**不追加任何 header** |
| 全类型兼容 | HTTP 全方法 / SSE 流式 / WebSocket 双向隧道 |
| 来源统计 | IP + 掩码 key + 状态码 + 成功率 + 时间窗口 + Top N（不泄露隐私） |
| 持久化 | 统计可存磁盘，重启不丢 |
| 轻量 | 单二进制 ~6MB，常驻内存 ~6MB，无运行时依赖 |
| Key 注入 | `/k/{alias}/` 模式，真实 key 只存服务端，用户无需带 key |
| 用量限额 | 按窗口限制 token 和请求次数（自动滚动重置） |
| 费用计算 | 自动识别模型，按官方定价表计算（含缓存折扣） |
| 禁止时段 | 按北京时间配置每天禁止使用的时段 |
| 费用乘数 | 按模型名+域名通配符匹配，用量和费用 ×N |
| 管理界面 | Web UI：Dashboard / 日志 / Key 管理 / 域名白名单 |

## 架构

所有请求在代理转发前经过统一拦截器链审查：

```
                    ┌─────────────────────────────────┐
                    │  请求到达 → 路由分发               │
                    │  / (透传) 或 /k/{alias}/ (注入)   │
                    └──────────────┬──────────────────┘
                                   │
                      ┌────────────▼────────────┐
                      │  [410] 过期检查           │
                      │  [403] 禁止时段           │
                      │  [402] 用量限额           │
                      │  [429] 频率限制           │
                      │  [403] 域名白名单         │
                      │  checkSetup → 收集规则    │
                      └────────────┬────────────┘
                                   │ 全部通过
                      ┌────────────▼────────────┐
                      │  newProxyHandler          │
                      │  转发 + 提取用量 + 乘数   │
                      │  + 统计记录               │
                      └─────────────────────────┘
```

每个拦截器实现统一的 `CheckFunc` 接口，新增检查只需写一个函数并加入数组。

详见 **[USAGE.md](USAGE.md)**。

## 部署方式（详见 [USAGE.md](USAGE.md)）

| 方式 | 命令 |
|------|------|
| Docker | `docker run -d -p 8080:8080 ghcr.io/dyyz1993/llm-http-proxy` |
| Compose | `docker compose up -d` |
| 二进制 | 从 [Releases](https://github.com/dyyz1993/llm-http-proxy/releases) 下载 |
| 源码 | `go run main.go` |

License: MIT
