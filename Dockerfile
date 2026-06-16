# ---- 构建阶段 ----
FROM golang:1.25-alpine AS builder

WORKDIR /src

# 先拷依赖文件,利用层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷源码
COPY . .

# 纯静态编译(CGO 关闭),注入版本号和编译时间,产出 /out/llm-http-proxy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=docker -X main.buildTime=$(TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S%:z)" \
    -o /out/llm-http-proxy .

# ---- 运行阶段 ----
# 用 alpine 而非 scratch:代理要转发 https:// 上游,必须有 CA 根证书。
FROM alpine:3.20

# 装 CA 证书(HTTPS 转发必需)+ ca-certificates 工具;清理 apk 缓存
RUN apk add --no-cache ca-certificates

# 非 root 运行,更安全
RUN adduser -D -u 10001 app
USER app
WORKDIR /app

# 从构建阶段拷贝二进制
COPY --from=builder /out/llm-http-proxy /app/llm-http-proxy

# 默认监听 8080
ENV ADDR=:8080
EXPOSE 8080

ENTRYPOINT ["/app/llm-http-proxy"]
CMD ["-addr", ":8080"]
