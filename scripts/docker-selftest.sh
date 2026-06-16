#!/usr/bin/env bash
# Docker CI 自验证脚本:经容器代理发请求,校验透传是否正确。
# 依赖:echo-backend.py 已在 8910 运行,代理容器已映射到 18080。
set -euo pipefail

RESP=$(curl -s "http://127.0.0.1:18080/http://host.docker.internal:8910/echo" \
  -H "Authorization: Bearer ci-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"hello":"docker"}')

echo "代理返回: $RESP"

echo "$RESP" | python3 -c '
import sys, json
d = json.load(sys.stdin)
assert d["method"] == "POST", "method 错: " + repr(d)
assert d["path"] == "/echo", "path 错: " + repr(d)
assert d["auth"] == "Bearer ci-secret-token", "Authorization 未透传: " + repr(d)
assert d["body"] == "{\"hello\":\"docker\"}", "body 错: " + repr(d)
print("OK: method/path/auth/body 全部正确透传")
'
