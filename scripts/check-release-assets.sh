#!/usr/bin/env bash
# check-release-assets.sh —— 发布资产闸门:造完包、**上传到 release 之前**断言
# dist/ 里的资产齐不齐、包内版式对不对。任一项不满足即退出 1,并逐条打印缺什么。
#
# 用法: check-release-assets.sh <dist-dir> <version>
#   <dist-dir>:goreleaser 的产物目录(通常是 ./dist)
#   <version>  :版本号,通常不带 v(如 0.1.0);snapshot 跑法传什么都能命中
#
# 匹配口径(前缀 + 精确,两档):
#   - 先按"前缀"找候选:文件名形如 daedalus-agent_<token>_linux_<arch>.tar.gz,
#     <token> 与传入的 <version> 相等、或两者互为前缀 → 命中。这样 goreleaser 的
#     snapshot 版本形态(随版本变,本仓不写死它)不必钉进脚本与测试。
#   - 但当传入的 <version> 里**不含 snapshot**(它是一个精确的发布版本号)时,
#     只认文件名与 daedalus-agent_<version>_linux_<arch>.tar.gz 完全相等 ——
#     否则 "0.1.0" 会把 "0.1.0-SNAPSHOT-xxx" 的快照包当成正片命中(假绿)。
#
# 包内版式(按 tar -tzf 的完整路径匹配,不靠解包后猜):
#   每个 tarball 里必须同时有 <包名>/bin/daedalus-agent 与 <包名>/scripts/install.sh
#   ——install.sh 随包走,机器上不再散落脚本;靶场也靠 '*/scripts/*' 找它。
#
# 退出码:0 = 全齐;1 = 有缺项(逐条打印缺什么,不笼统说"资产不全")。
set -uo pipefail

DIST="${1:-}"
VERSION="${2:-}"

if [ -z "$DIST" ] || [ -z "$VERSION" ]; then
    echo "用法: check-release-assets.sh <dist-dir> <version>" >&2
    exit 1
fi
if [ ! -d "$DIST" ]; then
    echo "check-release-assets: 缺 dist 目录: ${DIST}" >&2
    exit 1
fi

missing=0

# report <消息> —— 记一条缺项(打印到 stdout,便于 CI 日志与自检抓取)。
report() {
    echo "check-release-assets: $*"
    missing=1
}

# version_is_snapshot <version> —— 该版本号是不是 snapshot 形态(大小写无关)。
version_is_snapshot() {
    case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
        *snapshot*) return 0 ;;
        *) return 1 ;;
    esac
}

# token_matches <token> <version> —— 该候选包的版本 token 是否算命中。
#   - 完全相等 → 命中;
#   - 只有 snapshot 形态的版本号才允许"互为前缀"这种宽松匹配;
#     精确版本号(不含 snapshot)必须完全相等,否则快照包会冒充正片(假绿)。
token_matches() {
    local token="$1" version="$2"
    if [ "$token" = "$version" ]; then
        return 0
    fi
    version_is_snapshot "$version" || return 1
    local t v
    t="$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]')"
    v="$(printf '%s' "$version" | tr '[:upper:]' '[:lower:]')"
    case "$v" in "$t"*) return 0 ;; esac
    case "$t" in "$v"*) return 0 ;; esac
    return 1
}

# --- 逐个 arch 检查资产与包内布局 -------------------------------------------

for arch in amd64 arm64; do
    want="daedalus-agent_${VERSION}_linux_${arch}.tar.gz"
    hit=""
    near_miss=""

    # 候选:daedalus-agent_*_linux_<arch>.tar.gz;逐个抽出版本 token 比对。
    for candidate in "${DIST}"/daedalus-agent_*_linux_"${arch}".tar.gz; do
        [ -f "$candidate" ] || continue
        base="$(basename "$candidate")"
        token="${base#daedalus-agent_}"
        token="${token%_linux_${arch}.tar.gz}"
        if token_matches "$token" "$VERSION"; then
            hit="$base"
            break
        fi
        near_miss="${near_miss} ${base}"
    done

    if [ -z "$hit" ]; then
        if [ -n "$near_miss" ]; then
            report "缺资产 ${arch}: 期望 ${want};dist 里只有:${near_miss}(版本不匹配,不计入)"
        else
            report "缺资产 ${arch}: 期望 ${want}(dist 里没有)"
        fi
        continue
    fi

    # 包内版式:必须同时有 bin/daedalus-agent 与 scripts/install.sh。
    listing="$(tar -tzf "${DIST}/${hit}" 2>/dev/null)"
    if ! printf '%s\n' "$listing" | grep -Eq '(^|/)bin/daedalus-agent$'; then
        report "包内缺 bin/daedalus-agent: ${hit}"
    fi
    if ! printf '%s\n' "$listing" | grep -Eq '(^|/)scripts/install\.sh$'; then
        report "包内缺 scripts/install.sh: ${hit}"
    fi
done

# --- 校验和文件 --------------------------------------------------------------

if [ ! -f "${DIST}/SHA256SUMS" ]; then
    report "缺 SHA256SUMS: ${DIST}/SHA256SUMS"
fi

if [ "$missing" -ne 0 ]; then
    echo "check-release-assets: FAIL (${VERSION})"
    exit 1
fi

echo "check-release-assets: OK (${VERSION})"
exit 0
