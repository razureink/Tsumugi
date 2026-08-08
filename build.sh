#!/usr/bin/env bash
# 构建 Docker 镜像，并把编译出的二进制安装为系统全局命令（/usr/local/bin）。
set -e

OUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUT_DIR"' EXIT

echo "[1/3] docker build --target binaries"
docker build --target binaries --output "type=local,dest=$OUT_DIR" .

echo "[2/3] 安装到 /usr/local/bin"
for bin in tsumugi tsumugi-cli; do
  if [ -w /usr/local/bin ]; then
    install -m 0755 "$OUT_DIR/$bin" "/usr/local/bin/$bin"
  else
    sudo install -m 0755 "$OUT_DIR/$bin" "/usr/local/bin/$bin"
  fi
done

echo "[3/3] 完成"
command -v tsumugi
command -v tsumugi-cli