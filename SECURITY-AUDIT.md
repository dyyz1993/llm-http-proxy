# llm-http-proxy 安全审查文档

> 本文档供第三方安全审查使用。包含：系统架构说明 + 每一项的可执行验证步骤。
> 审查者无需任何特权，凭公开仓库 + 对外 SSH 即可完成全部检查。

---

## 一、这是什么

一个反向代理服务，给 LLM API（GLM/OpenAI/Claude 等）做透明转发。开源在：
**https://github.com/dyyz1993/llm-http-proxy**

两个部署形态（同一份代码）：
- **jd 服务器**：纯 Go 二进制 + systemd
- **jkj（NAS）**：Docker 容器（`ghcr.io/dyyz1993/llm-http-proxy:ssh`），带对外 SSH 访问

---

## 二、架构与信任边界

```
┌─────────────────────────────────────────────────────────────┐
│  GitHub 仓库(公开,可审查代码)                                │
│  - main.go / stats.go  代理代码                              │
│  - Dockerfile / Dockerfile.ssh  镜像构建                     │
│  - .github/workflows/  CI/CD 配置                            │
└────────────────────┬────────────────────────────────────────┘
                     │ 打 tag (v*)
                     ▼
┌─────────────────────────────────────────────────────────────┐
│  GitHub Actions (CI,在 GitHub 云端运行)                      │
│  1. 编译 Go 二进制(从源码,无外部二进制下载)                  │
│  2. 构建 Docker 镜像(基于 golang:1.25-alpine + debian-slim) │
│  3. 推送到 GHCR(ghcr.io/dyyz1993/llm-http-proxy)           │
│  4. 通过 SSH(deploy key)连服务器,拉取镜像/二进制并重启       │
└────────────────────┬────────────────────────────────────────┘
                     │ SSH deploy key(仅 CI 持有,不在服务器上)
            ┌────────┴────────┐
            ▼                 ▼
    ┌──────────────┐  ┌───────────────────────────────┐
    │  jd 服务器    │  │  jkj NAS(Docker)              │
    │  纯二进制     │  │  容器:llm-proxy-box            │
    │  systemd 管理 │  │  - 代理服务(8080→对外 9094)    │
    │               │  │  - SSH(22→对外 22022)          │
    │               │  │    · guest: sftp-only(公开)    │
    │               │  │    · deploy: bash(管理)        │
    └──────────────┘  └───────────────────────────────┘
```

**关键信任边界：**
1. **CI 的 deploy key** 存在 GitHub secret 里，**不在 jd/jkj 服务器上**。通过对外 SSH（guest/deploy）拿不到这个 key。
2. **对外 SSH（22022）的 guest** 是 sftp-only + chroot，**碰不到宿主机 NAS**。
3. **部署（CI→服务器）** 和 **对外访问（用户→SSH/代理）** 是两条完全隔离的路径。

---

## 三、检查清单（可执行）

### A. 代理代码后门检查

**目标**：确认代码不会偷传数据、不留后门、不记录明文 key。

**A1. 代码只有转发逻辑,无额外外发**

```bash
# 克隆代码
git clone https://github.com/dyyz1993/llm-http-proxy
cd llm-http-proxy

# 搜所有网络调用(除转发外的)
grep -rn "http.Get\|http.Post\|http.DefaultClient\|http.NewRequest" *.go | grep -v _test.go
# 预期:只有 NewRequestWithContext(转发用)和 NewConfig(WS 测试)
# 不应有向固定外部地址上报的调用

# 搜可疑的硬编码 URL / IP
grep -rn "http://\|https://" *.go | grep -v _test.go | grep -v "// "
# 预期:只有注释里的示例 URL,无隐藏的外联地址
```

**A2. key 掩码,不记录明文**

```bash
# 看 stats.go 里 key 怎么处理
grep -n "maskKey\|maskedKey\|extractKey" stats.go
# 预期:key 提取后立即 maskKey() 掩码,只在掩码后存储/日志
# 日志格式: log.Printf("req ip=%s key=%s ...") ← key 是掩码后的
```

**A3. 无可疑的 init/后台 goroutine**

```bash
# 搜 go func() 启动的后台任务
grep -n "go func\|go [a-z]" *.go | grep -v _test.go
# 预期:只有 startPersistLoop(统计落盘)和 WS 双向拷贝
# 不应有连到外部的心跳/上报
```

**A4. 依赖检查（供应链）**

```bash
cat go.mod
# 预期:依赖极少,主要是 golang.org/x/net(WebSocket 库)
# 无可疑的第三方包

# 检查依赖来源
go mod graph | head
```

---

### B. SSH 隔离 / 越权检查

**目标**：确认通过对外 SSH（22022）的 guest 用户无法逃逸、无法碰 NAS。

**连接信息**（审查者可用）：
- 地址：`llm-ssh.19930810.xyz:22022`
- guest 账户：`guest` / 密码 `guest123`（公开，只读）

**B1. guest 拿不到 shell**

```bash
# 尝试 SSH 执行命令(应被拒)
ssh -p 22022 guest@llm-ssh.19930810.xyz 'whoami'
# 预期: "This service allows sftp connections only."
# 退出码非 0

# 尝试交互式 shell
ssh -p 22022 guest@llm-ssh.19930810.xyz
# 预期: 直接断开,无 shell
```

**B2. guest 只能 sftp,且被 chroot**

```bash
# sftp 连接,看能看到什么
sftp -P 22022 guest@llm-ssh.19930810.xyz
# 登录后 ls,预期只看到: source/ binary/ deploy/ logs/
# 这是 chroot 后的 /public 目录

# 尝试访问 chroot 外(应失败)
sftp> cd /etc
# 预期: "File /etc not found" 或类似(chroot 挡住)

sftp> get /etc/passwd
# 预期: 失败
```

**B3. guest 无法端口转发（逃逸常用手段）**

```bash
# 尝试 SSH 端口转发(应被拒)
ssh -p 22022 -L 9999:localhost:22 guest@llm-ssh.19930810.xyz
# 预期: 因 AllowTcpForwarding no,转发失败

# 尝试 socks 代理
ssh -p 22022 -D 1080 guest@llm-ssh.19930810.xyz
# 预期: 失败
```

**B4. 容器隔离（从宿主视角,需服务器权限）**

```bash
# 在 jkj 上执行(审查者若无宿主权限,可请管理员提供 docker inspect 输出)
docker inspect llm-proxy-box --format '
Privileged: {{.HostConfig.Privileged}}
Networks: {{.HostConfig.NetworkMode}}
PidMode: {{.HostConfig.PidMode}}
CapAdd: {{.HostConfig.CapAdd}}
SecurityOpt: {{.HostConfig.SecurityOpt}}
'
# 预期:
#   Privileged: false  ← 非特权
#   Network: llm-proxy-box_llm_proxy_net  ← 独立网络
#   无 CapAdd, 无 SecurityOpt 放宽

# 检查有没有挂 docker.sock(逃逸到宿主的关键)
docker inspect llm-proxy-box --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'
# 预期:只有 ./data 和 ./logs,无 /var/run/docker.sock
```

---

### C. CI / 部署链路审查

**目标**：确认 CI 不会泄露 secret、deploy key 不被外人触发。

**C1. CI 配置全公开**

```bash
# 所有 workflow 文件都在仓库里,可审查
ls .github/workflows/
# ci.yml       - 测试(每次 push)
# release.yml  - 构建+发布(打 tag 触发)
# deploy.yml   - 部署到服务器(release 完成后)

# 检查 deploy.yml 用的 secret
cat .github/workflows/deploy.yml | grep -A1 "secrets\."
# 预期:JD_SSH_KEY / JD_HOST / JD_USER
# 这些是 GitHub Encrypted Secrets,值不会出现在代码/日志里
```

**C2. secret 不会泄露**

- GitHub Actions 的 secret 是**加密存储**的，运行时才解密注入环境变量
- 日志里 secret 值会被自动屏蔽成 `***`
- **但仓库是 public** —— 任何能 push 到 main 的人理论上能写个 workflow 把 secret 打印出来
- **缓解**：deploy key 是**专用 ed25519 key**（非日常 SSH key），且只授权在 jkj/jd 上：
  - 即使泄露，只影响这两台服务器
  - 可随时从服务器的 authorized_keys 删除该 key 吊销

**C3. CI 构建可复现**

```bash
# 镜像是 CI 从源码构建的,可本地复现验证一致性
git clone https://github.com/dyyz1993/llm-http-proxy
cd llm-http-proxy
git checkout v1.8.0
docker build -f Dockerfile.ssh -t llm-http-proxy:audit .

# 对比镜像内容(应与 GHCR 的一致)
docker history llm-http-proxy:audit --no-trunc
```

---

### D. 镜像 / 供应链审查

**D1. 基础镜像可信**

```bash
# 看 Dockerfile.ssh 的 FROM
head -1 Dockerfile.ssh   # builder
grep "^FROM" Dockerfile.ssh
# 预期:
#   FROM golang:1.25-alpine AS builder  (官方 Go 镜像)
#   FROM debian:bookworm-slim           (官方 Debian)
# 无可疑的第三方基础镜像
```

**D2. 镜像内安装的包**

```bash
# Dockerfile.ssh 里 apt-get install 的内容
grep -A5 "apt-get install" Dockerfile.ssh
# 预期:只有 openssh-server supervisor ca-certificates
# 无可疑的后门工具
```

**D3. 镜像扫描**

```bash
# 用 trivy 扫描已知漏洞(审查者本地跑)
trivy image ghcr.io/dyyz1993/llm-http-proxy:latest-ssh
# 预期:基础镜像的常规漏洞(CVE),无高危后门
```

---

## 四、已知限制与说明（诚实声明）

以下是审查者应知道的局限，不是隐瞒，是明确边界：

1. **仓库是 public**：任何人能读代码、读 CI 配置。但 **deploy key / 密码不在仓库里**（在 GitHub secret / .env）。

2. **guest 密码公开**（`guest123`）：这是设计如此（公开只读访问）。guest 被 chroot + sftp-only 限制，即使密码公开也碰不到 NAS。**deploy 密码不公开**。

3. **持久化统计文件**（`/data/stats.json`）：记录掩码后的 key 和 IP。**不记录明文 key、不记录 body/path/query**。可审查 stats.go 的 `record()` 函数确认。

4. **日志**：每请求一行，含 `ip= 掩码key= host= status=`，**不含 body**。可通过 `journalctl`(jd) 或 `docker logs`(jkj) 查看。

5. **CI deploy key 的风险**：public 仓库下，能 push 的人理论上能窃取 secret。当前依赖"能 push 的人可信"。更严格的做法是加 branch protection / 用 OIDC，目前未做。

---

## 五、如何报告问题

发现任何安全问题，请在 GitHub 提 issue：
https://github.com/dyyz1993/llm-http-proxy/issues

或联系仓库所有者。

---

## 附录：快速验证脚本

审查者可一键跑以下检查（只需公开信息 + guest SSH）：

```bash
#!/bin/bash
# 安全快速检查脚本
echo "=== 1. 代码无额外外发 ==="
git clone -q https://github.com/dyyz1993/llm-http-proxy /tmp/lhp-audit && cd /tmp/lhp-audit
grep -rn "http.Get\|http.Post" *.go | grep -v _test.go | grep -v "outReq\|NewRequest" && echo "⚠️ 有额外外发" || echo "✅ 无额外外发"

echo "=== 2. guest 拿不到 shell ==="
ssh -o ConnectTimeout=8 -p 22022 guest@llm-ssh.19930810.xyz 'id' 2>&1 | grep -q "sftp only" && echo "✅ guest 无 shell" || echo "⚠️ guest 可能拿到 shell"

echo "=== 3. guest chroot 限制 ==="
echo "ls /etc" | sftp -P 22022 -oBatchMode=no guest@llm-ssh.19930810.xyz 2>&1 | grep -q "not found" && echo "✅ chroot 生效" || echo "⚠️ 可能逃逸"

echo "=== 4. CI 配置公开可审 ==="
ls .github/workflows/ && echo "✅ CI 全公开"
```
