#!/bin/bash
set -e

# ==========================================
# 初始化 SSH 隔离盒子
#
# 环境变量:
#   GUEST_PASSWORD  - guest 用户密码(公开共享,只读 sftp)
#   DEPLOY_PASSWORD - deploy 用户密码(你的管理密码)
#   DEPLOY_SSH_KEY  - deploy 的公钥(可选,推荐用 key 免密)
# ==========================================

# --- 1. 设置用户密码 ---
# guest: 默认密码 "guest"(公开),可用 GUEST_PASSWORD 覆盖
GUEST_PASSWORD="${GUEST_PASSWORD:-guest}"
echo "guest:${GUEST_PASSWORD}" | chpasswd

# deploy: 默认密码 "deploy"(请改!),可用 DEPLOY_PASSWORD 覆盖
DEPLOY_PASSWORD="${DEPLOY_PASSWORD:-deploy}"
echo "deploy:${DEPLOY_PASSWORD}" | chpasswd

# --- 2. 注入 deploy 的 SSH 公钥(如提供) ---
if [ -n "$DEPLOY_SSH_KEY" ]; then
    mkdir -p /home/deploy/.ssh
    echo "$DEPLOY_SSH_KEY" > /home/deploy/.ssh/authorized_keys
    chmod 700 /home/deploy/.ssh
    chmod 600 /home/deploy/.ssh/authorized_keys
    chown -R deploy:deploy /home/deploy/.ssh
    echo "[entrypoint] deploy SSH key injected"
fi

# --- 3. 生成 SSH host key(首次启动) ---
if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
    ssh-keygen -t ed25519 -f /etc/ssh/ssh_host_ed25519_key -N '' -q
fi
if [ ! -f /etc/ssh/ssh_host_rsa_key ]; then
    ssh-keygen -t rsa -f /etc/ssh/ssh_host_rsa_key -N '' -q
fi

# --- 4. 确保 /public/logs 存在且 guest 可读 ---
mkdir -p /public/logs /data
# proxy.log 让 guest 能读(看运行日志)
touch /public/logs/proxy.log /public/logs/proxy-error.log
chmod 644 /public/logs/proxy.log /public/logs/proxy-error.log 2>/dev/null || true

# --- 5. 写部署说明(给访客看)---
cat > /public/deploy/README.txt << EOF
llm-http-proxy 部署说明
=======================

二进制位置:  /usr/local/bin/llm-http-proxy
版本:        $(/usr/local/bin/llm-http-proxy -version 2>/dev/null || echo unknown)
监听端口:    8080
持久化:      /data/stats.json

管理(仅 deploy 用户):
  supervisorctl status              - 查看服务状态
  supervisorctl restart llm-http-proxy  - 重启代理

统计端点(HTTP):
  /__stats   - 请求统计
  /__version - 版本信息

源码: /public/source/
日志: /public/logs/
EOF
chmod 644 /public/deploy/README.txt

echo "[entrypoint] guest (sftp-only, password=$GUEST_PASSWORD)"
echo "[entrypoint] deploy (bash, password=$DEPLOY_PASSWORD)"
echo "[entrypoint] starting supervisor..."

exec "$@"
