# Tsumugi 内存数据库 — 多阶段构建
# 构建阶段：编译 Go 二进制
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /build/tsumugi .
RUN CGO_ENABLED=0 go build -o /build/tsumugi-cli ./cmd/tsumugi-cli

# 导出阶段：仅含两个可执行文件，供宿主机全局安装
FROM scratch AS binaries
COPY --from=builder /build/tsumugi /tsumugi
COPY --from=builder /build/tsumugi-cli /tsumugi-cli

# 运行阶段：最小化运行镜像（compose 默认构此阶段）
FROM alpine:3.20
WORKDIR /app
COPY --from=builder /build/tsumugi /usr/local/bin/tsumugi
COPY --from=builder /build/tsumugi-cli /usr/local/bin/tsumugi-cli
# 以非 root 运行：进程持有数据卷与配置写权限，不应需要 root
RUN adduser -D -H -u 10001 tsumugi && mkdir -p /app/data /app/config /app/backup && chown -R tsumugi /app
USER tsumugi
# 端口与默认配置一致：二进制 9999、监控/管理面板 10232、MySQL 兼容 3309
EXPOSE 9999 10232 3309
VOLUME ["/app/data", "/app/config", "/app/backup"]
ENTRYPOINT ["tsumugi"]
