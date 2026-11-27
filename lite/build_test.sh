#!/usr/bin/env bash
# 全测脚本：格式检查 + 静态检查 + 全量单元测试 + 构建。串行执行，避免并发编译超时。
set -euo pipefail
cd "$(dirname "$0")"

echo "=== [1/4] gofmt 格式检查 ==="
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "以下文件未按 gofmt 格式化，请执行 gofmt -w *.go："
  echo "$unformatted"
  exit 1
fi

echo "=== [2/4] go vet 静态检查 ==="
go vet ./...

echo "=== [3/4] go test 全量单元测试 ==="
go test . -count=1 -timeout 120s

echo "=== [4/4] go build 构建可执行程序 ==="
mkdir -p bin
go build -o bin/dbmigration-lite .

echo "=== 全部通过 ==="
