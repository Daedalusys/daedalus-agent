#!/usr/bin/env bash
# 输出 SDK checkout 的 ref(vX.Y.Z),取自本仓 go.mod 的 require。
#
# 为什么必须失败而不是回落:agent 的 release 资产是**带 attestation 的出货二进制**,
# 它会装到用户机器上。2026-11-17 之前 release.yml / ci.yml 检出 SDK 不带 ref,
# 等于「v0.1.0 的 tarball 由当时 SDK main 构建」—— 事后无法重建同一份字节,
# attestation 证明的只是一个无法复现的构建。
#
# 用法: sdk-pin.sh          # 打印 vX.Y.Z;占位/缺失即 exit 1
#       sdk-pin.sh --check  # 只校验,不打印
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
GOMOD="$ROOT/go.mod"

[ -f "$GOMOD" ] || { echo "错误: 找不到 $GOMOD" >&2; exit 1; }

if grep -qF 'v0.0.0-00010101000000-000000000000' "$GOMOD"; then
    echo "错误: go.mod 的 SDK require 仍是占位伪版本(v0.0.0-00010101000000-...)。" >&2
    echo "      agent 的出货二进制不得基于浮动 SDK 构建:请改成已发布的 vX.Y.Z 版本。" >&2
    exit 1
fi

v="$(sed -n 's/.*Daedalusys\/daedalus-sdk v\([0-9.]*\).*/\1/p' "$GOMOD" | head -n1)"
if [ -z "$v" ] || [ "$v" = "0.0.0" ]; then
    echo "错误: go.mod 里找不到 SDK 的语义化版本(require 行:'$v')。" >&2
    exit 1
fi
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "错误: SDK 版本形态非法:'$v'(需 x.y.z)" >&2
    exit 1
}

if [ "${1:-}" = "--check" ]; then
    echo "OK: SDK 钉在 v$v"
else
    echo "v$v"
fi
