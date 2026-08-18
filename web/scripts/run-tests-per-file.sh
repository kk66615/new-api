#!/usr/bin/env bash
# 逐文件运行前端测试，每个文件一个 bun 进程。
#
# 为什么不直接 `bun test`：Bun 的 node:test 兼容层带跨文件的全局状态，多个文件在同
# 一进程里并发时，前一个文件的异步用例尚未结束，后一个文件的顶层 describe() 会被误
# 判为嵌套在 test() 内并抛 NotImplementedError（oven-sh/bun#5090）。命中的文件里所有
# 用例都不会注册，汇总只多出几条 error，看不出整份文件被跳过。仓库 31 个测试文件中
# 30 个用 node:test，任何文件都可能在某次调度里中招。
#
# 迁移到 vitest 后可以删掉本脚本，直接跑 `bun run test`。
set -uo pipefail

cd "$(dirname "$0")/.."

total=0
failed=0
failed_list=''

while IFS= read -r f; do
  total=$((total + 1))
  echo "::group::$f"
  if ! bun test "$f"; then
    failed=$((failed + 1))
    failed_list="$failed_list$f"$'\n'
  fi
  echo "::endgroup::"
done < <(find src -type f \( -name '*.test.ts' -o -name '*.test.tsx' \) | sort)

if [ "$total" -eq 0 ]; then
  echo "未找到任何测试文件，视为失败（避免静默通过）" >&2
  exit 1
fi

echo
echo "共 $total 个测试文件，失败 $failed 个"
if [ "$failed" -gt 0 ]; then
  printf '%s' "$failed_list"
  exit 1
fi
