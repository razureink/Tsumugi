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
# 暴露二进制协议端口与监控/管理面板端口
EXPOSE 9999 8080 3306
VOLUME ["/app/data"]
ENTRYPOINT ["tsumugi"]
