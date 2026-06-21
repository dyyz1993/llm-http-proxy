# agents.md — 协作约定（给 AI 助手 / 协作者看）

> 这份文件只记录"怎么协作、怎么发布"的流程约定，不改代码行为。
> 代码细节看源码注释和 README，部署配置看 `.github/workflows/`。

---

## 一、项目是什么

`llm-http-proxy`:百分百透传的反向代理(GLM / OpenAI / Claude 等 LLM API)。
- HTTP / SSE / WebSocket 全透传,不追加任何 header。
- 可选 key 注入模式:`/k/{alias}/目标URL`,真实 key 只存在服务端。
- IP 来源统计(北京时间)、Web 管理界面(单密码登录)。

代码在 `/Users/xuyingzhou/Project/temporary/LLM-HTTP-proxy`。

---

## 二、本地开发环境(重要!)

这台机器上 **有两个 Go**,别用错:

| 用途 | 路径 | 架构 | 能用吗 |
|------|------|------|--------|
| Homebrew | `/usr/local/bin/go` (1.26.3) | **amd64**(错的,机器是 arm64) | ❌ 缺 compile/vet 工具 |
| gvm | `~/.gvm/gos/go1.23.8/bin/go` | arm64 ✅ | ✅ 用这个 |

**所有 go 命令(build / test / fmt)必须用 gvm 的 go:**

```bash
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH go build ./...
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH go test ./...
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH go vet ./...
```

git 的 pre-commit / pre-push hook 内部会调 `go vet` / `go test -race`,
**如果不带 gvm 的 PATH,hook 会因为 "no such tool vet" 失败,导致 commit/push 被拒。**

所以 `git commit` 和 `git push` 也要带这串 PATH:

```bash
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH git commit -m "..."
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH git push origin main
```

commit 前如果 admin.go 之类没 gofmt,hook 会拦,先跑:
```bash
~/.gvm/gos/go1.23.8/bin/gofmt -w <文件>
```

---

## 三、发布流程(分阶段发布 — 务必遵守!)

> **核心原则:先发 jd,验证没问题,再发 NAS。绝不同时上两台。**

GitHub 有三个 workflow:
- `release.yml` — 打 `v*` tag 自动触发,构建多平台二进制 + Docker 镜像,发 Release。
- `deploy.yml` — Release 完成后自动触发,**只自动部署 jd**;NAS 改为手动。
- `ci.yml` — PR / push 跑测试。

### 正常发布步骤

```bash
# 1. 代码先过本地测试(gvm 的 go!)
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH go test ./...

# 2. 提交 + 推送(gvm PATH,否则 hook 拦)
git add ...
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH git commit -m "..."
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH git push origin main

# 3. 打 tag(版本号递增,如 v2.1.3)→ 推 tag
git tag v2.1.3
GOROOT=~/.gvm/gos/go1.23.8 PATH=~/.gvm/gos/go1.23.8/bin:$PATH git push origin v2.1.3

# 4. 这时会自动:release.yml 构建 → deploy.yml 自动部署 jd
#    监控进度:
gh run list --workflow=release.yml --limit=1
gh run list --workflow=deploy.yml --limit=1

# 5. jd 部署完,验证 OK 后,再去 GitHub Actions 页面手动触发 NAS 部署:
#    Deploy workflow → Run workflow → target=jkj,tag=v2.1.3
#    或命令行:
gh workflow run deploy.yml -f target=jkj -f tag=v2.1.3
```

### 为什么不全自动上两台

用户(项目 owner)明确要求:**先 jd,没问题再 NAS**。
所以 `deploy.yml` 里 jkj 那个 job 的触发条件被设成只能手动
(`workflow_dispatch` + `inputs.target == 'jkj'`)。
打 tag 时只自动跑 jd。**不要改回全自动,除非 owner 明确说要改。**

---

## 四、服务器信息

| 名字 | 角色 | 地址 | 二进制路径 | 重启方式 |
|------|------|------|-----------|---------|
| jd | 公网 VPS (amd64) | `root@36.151.142.174:22` | `/usr/local/bin/llm-http-proxy` | `systemctl restart llm-http-proxy` |
| jkj | NAS 上的容器 (arm64) | `deploy@llm-ssh.19930810.xyz:22022` | `/opt/proxy/llm-http-proxy` | `supervisorctl restart llm-http-proxy` |

- jd 用 `ssh jd`(~/.ssh/config 里配好了)。
- jkj 是 NAS 上跑的独立 Docker 容器,SSH 进容器,不碰 NAS 宿主机。
- 公网入口:jd `:8080`,jkj `https://p.19930810.xyz:8443`(反代到容器)。

### 已知坑

- **jd 的 8080 验证偶尔返回 HTTP 000**:deploy.yml 最后那步 `curl :8080/__version`
  有时报红,但实际二进制已更新、服务 active。属于验证步的端口/防火墙问题,
  不代表部署失败。看日志里有没有"新版本: ... 服务已启动"判断真实结果。
- **jd SSH 偶发超时**:TCP 22 通但 sshd 无响应,可能是负载高或 fail2ban。
  需 owner 从 VPS 控制台处理,助手无权重启 jd。
- **scp 传大文件慢**:jkj 容器走 frpc 隧道,直接 scp 很慢,deploy.yml 用了
  gzip 压缩 + 容器内 curl 优先的策略。

---

## 五、不要做的事

- ❌ 不要手动 scp 编译产物去部署,有 CI 就走 CI。
- ❌ 不要让两台同时自动部署,保持"先 jd 后 NAS"。
- ❌ 不要改 NAS 宿主机的东西,只操作 jkj 容器内部。
- ❌ 不要用 Homebrew 的 go(`/usr/local/bin/go`),架构不对会报 "no such tool"。
- ❌ 不要绕过 git hooks(git commit/push 一定带 gvm 的 PATH 让 hook 跑通)。
- ❌ 不要把真实 API key 写进任何文档 / commit / 日志。
