# Tsumugi 内存数据库 — 多阶段构建
# 构建阶段：编译 Go 二进制
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /build/tsumugi .

# 运行阶段：最小化运行镜像
FROM alpine:3.20
WORKDIR /app
COPY --from=builder /build/tsumugi /app/tsumugi
# 暴露二进制协议端口与监控/管理面板端口
EXPOSE 9999 8080 3306
VOLUME ["/app/data"]
ENTRYPOINT ["/app/tsumugi"]
