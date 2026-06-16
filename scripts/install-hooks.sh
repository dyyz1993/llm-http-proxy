#!/usr/bin/env bash
# 安装 Git hooks:把 scripts/hooks/* 拷到 .git/hooks/ 并赋可执行权限。
# 用法: bash scripts/install-hooks.sh
set -e

cd "$(git rev-parse --show-toplevel)"
HOOKS_DIR=".git/hooks"
SRC_DIR="scripts/hooks"

mkdir -p "$HOOKS_DIR"

for hook in pre-commit pre-push; do
  src="$SRC_DIR/$hook"
  dst="$HOOKS_DIR/$hook"
  cp "$src" "$dst"
  chmod +x "$dst"
  echo "✅ 已安装 $hook -> $dst"
done

echo ""
echo "Git hooks 已就绪:"
echo "  pre-commit: 提交前跑 go vet + gofmt"
echo "  pre-push:   推送前跑 go test -race"
echo ""
echo "如需跳过(紧急情况): git commit --no-verify / git push --no-verify"
