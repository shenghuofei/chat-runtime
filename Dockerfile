# chat-runtime 多阶段构建 Dockerfile
# 阶段一：使用 golang:1.23-alpine 编译静态二进制
# 阶段二：使用 alpine:3.20 作为最小运行时镜像

# ============================================================
# 构建阶段
# ============================================================
FROM golang:1.23-alpine AS builder

# 构建参数：版本与构建时间（可由 CI 通过 --build-arg 注入）
ARG VERSION=dev
ARG BUILD_TIME=unknown

# 安装构建所需的基础工具（git 用于版本信息）
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 先拷贝依赖清单并下载，利用 Docker 层缓存加速后续构建
COPY go.mod go.sum* ./
RUN go mod download

# 拷贝源码并编译（CGO 关闭，生成静态二进制）
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X 'main.Version=${VERSION}' -X 'main.BuildTime=${BUILD_TIME}'" \
    -o /out/chat-runtime .

# ============================================================
# 运行阶段
# ============================================================
FROM alpine:3.20

# 安装 CA 证书（HTTPS 调用 LLM API 需要）与时区数据
RUN apk add --no-cache ca-certificates tzdata

# 从构建阶段拷贝二进制到标准可执行路径
COPY --from=builder /out/chat-runtime /usr/local/bin/chat-runtime

# 以非 root 用户运行，降低权限风险
RUN adduser -D -u 10001 appuser
USER appuser

WORKDIR /app

# Web 服务默认监听端口
EXPOSE 8080

# 默认以 Web 服务模式启动；可在 docker run 时追加参数（如 --config /app/config.yml）
ENTRYPOINT ["chat-runtime", "serve"]
