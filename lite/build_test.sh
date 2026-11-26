#!/usr/bin/env bash
# 全测脚本：静态检查 + 全量单元测试 + 构建。串行执行，避免并发编译超时。
set -euo pipefail
cd "$(dirname "$0")"

echo "=== [1/3] go vet 静态检查 ==="
go vet ./...

echo "=== [2/3] go test 全量单元测试 ==="
go test . -count=1 -timeout 120s

echo "=== [3/3] go build 构建可执行程序 ==="
mkdir -p bin
go build -o bin/dbmigration-lite .

echo "=== 全部通过 ==="
