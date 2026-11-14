#!/usr/bin/env bash
# check-release-assets-test.sh —— check-release-assets.sh 的表驱动自检。
#
# 为什么要自检:发布资产闸门是"坏包不上 release"的最后一道防线,而它自己
# 不会被 CI 的单测覆盖(它跑在造包之后)。自检用 mktemp -d 造合成 dist 目录
# (tar -czf 造带 bin/ scripts/ 的包),逐个用例跑被测脚本,断言**退出码**与
# **提示文案**(文案要点名缺什么,否则运维看到"资产不全"还是不知道该查哪一项)。
#
# 用例 1-4 是主路径(齐全 / 缺 arch / 缺校验和 / 缺包内安装脚本);
# 用例 5-6 钉住匹配口径:精确版本号不得被 snapshot 包冒充(假绿),
# 而 snapshot 形态的版本号要能按前缀命中 —— 两档都不能偏。
#
# 全程无副作用:只读仓库里的被测脚本,产物全在临时目录,不碰仓库的 dist/。
#
# 用法: bash scripts/check-release-assets-test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="${SCRIPT_DIR}/check-release-assets.sh"

# 自检用的版本号:必须是"不带 v"的形态(与 goreleaser 的 {{ .Version }} 同形),
# 刻意取一个仓库里不存在的版本,免得误判到真产物。
VERSION="9.9.9"

fail_n=0
pass_n=0

# 被测脚本还不存在时:这是"红灯"的正常形态,立刻报失败(只记 1 条,别把
# 六个用例重复计成六条噪声)。
if [ ! -f "$SUBJECT" ]; then
    echo "被测脚本不存在: ${SUBJECT}"
    echo "RESULT: SELFTEST FAIL (1)"
    exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- 造合成产物 -------------------------------------------------------------

# stage_pkg <stage-root> <带 install.sh:0|1>
# 造一个包内布局:<包名>/bin/daedalus-agent(+ <包名>/scripts/install.sh)。
stage_pkg() {
    local root="$1" with_install="$2"
    mkdir -p "$root/pkg/bin"
    printf 'fake binary\n' >"$root/pkg/bin/daedalus-agent"
    chmod +x "$root/pkg/bin/daedalus-agent"
    if [ "$with_install" = "1" ]; then
        mkdir -p "$root/pkg/scripts"
        printf '#!/usr/bin/env bash\n# fake installer\n' >"$root/pkg/scripts/install.sh"
        chmod +x "$root/pkg/scripts/install.sh"
    fi
}

# make_tarball <distdir> <token> <arch> <带 install.sh:0|1>
# 造 daedalus-agent_<token>_linux_<arch>.tar.gz;token 可刻意与 VERSION 不同,
# 用来模拟 snapshot 版本形态(不在测试里写死 goreleaser 的具体形态)。
make_tarball() {
    local dist="$1" token="$2" arch="$3" with_install="$4" stage
    stage="$(mktemp -d)"
    stage_pkg "$stage" "$with_install"
    tar -czf "${dist}/daedalus-agent_${token}_linux_${arch}.tar.gz" -C "$stage" pkg
    rm -rf "$stage"
}

# make_sums <distdir> —— 造一份 SHA256SUMS(内容不必真,闸门只看它在不在)。
make_sums() {
    local dist="$1"
    printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
        "daedalus-agent_${VERSION}_linux_amd64.tar.gz" >"${dist}/SHA256SUMS"
}

# new_dist —— 造一个空的合成 dist 目录。
new_dist() {
    local d
    d="$(mktemp -d -p "$TMP")"
    printf '%s' "$d"
}

# --- 断言 -------------------------------------------------------------------

# run_subject <distdir> [version] → 置全局 out(合并 stdout/stderr)与 rc(退出码)。
run_subject() {
    local dist="$1" version="${2:-$VERSION}"
    out="$(bash "$SUBJECT" "$dist" "$version" 2>&1)"
    rc=$?
}

# expect <名称> <期望退出码> <期望关键字>
expect() {
    local name="$1" want_rc="$2" want_kw="$3"
    if [ "$rc" != "$want_rc" ]; then
        printf '  FAIL  %s (期望退出码 %s,实际 %s)\n--- 被测输出 ---\n%s\n---\n' \
            "$name" "$want_rc" "$rc" "$out"
        fail_n=$((fail_n + 1))
        return
    fi
    if [ -n "$want_kw" ] && ! printf '%s' "$out" | grep -qF -- "$want_kw"; then
        printf '  FAIL  %s (输出不含关键字 %s)\n--- 被测输出 ---\n%s\n---\n' \
            "$name" "$want_kw" "$out"
        fail_n=$((fail_n + 1))
        return
    fi
    printf '  PASS  %s\n' "$name"
    pass_n=$((pass_n + 1))
}

echo "== check-release-assets-test.sh =="

# 用例1:三件齐全、包内布局正确 → exit 0。
d="$(new_dist)"
make_tarball "$d" "$VERSION" amd64 1
make_tarball "$d" "$VERSION" arm64 1
make_sums "$d"
run_subject "$d"
expect "三件齐全、包内布局正确 → exit 0" 0 "check-release-assets: OK"

# 用例2:缺 arm64 包 → exit 1,输出点名 arm64。
d="$(new_dist)"
make_tarball "$d" "$VERSION" amd64 1
make_sums "$d"
run_subject "$d"
expect "缺 arm64 包 → exit 1 且点名 arm64" 1 "arm64"

# 用例3:缺 SHA256SUMS → exit 1,输出点名 SHA256SUMS。
d="$(new_dist)"
make_tarball "$d" "$VERSION" amd64 1
make_tarball "$d" "$VERSION" arm64 1
run_subject "$d"
expect "缺 SHA256SUMS → exit 1 且点名 SHA256SUMS" 1 "SHA256SUMS"

# 用例4:tarball 里没有 scripts/install.sh → exit 1,输出点名 install.sh。
d="$(new_dist)"
make_tarball "$d" "$VERSION" amd64 0
make_tarball "$d" "$VERSION" arm64 0
make_sums "$d"
run_subject "$d"
expect "包内缺 scripts/install.sh → exit 1 且点名 install.sh" 1 "install.sh"

# 用例5(假绿守卫):传入精确版本 9.9.9,dist 里只有带 SNAPSHOT 后缀的包
# → 必须 exit 1。否则快照包会冒充正片,release 里可能传上未定版的产物。
d="$(new_dist)"
make_tarball "$d" "${VERSION}-SNAPSHOT-fake123" amd64 1
make_tarball "$d" "${VERSION}-SNAPSHOT-fake123" arm64 1
make_sums "$d"
run_subject "$d"
expect "精确版本不得被 SNAPSHOT 包冒充 → exit 1 且点名 amd64" 1 "amd64"

# 用例6(snapshot 容忍):传入带 snapshot 后缀的版本,资产是另一种 snapshot
# 形态 → 按前缀命中,exit 0(不在脚本/测试里写死 goreleaser 的具体形态)。
d="$(new_dist)"
make_tarball "$d" "${VERSION}-SNAPSHOT-fake123" amd64 1
make_tarball "$d" "${VERSION}-SNAPSHOT-fake123" arm64 1
make_sums "$d"
run_subject "$d" "${VERSION}-snapshot"
expect "snapshot 形态按前缀命中 → exit 0" 0 "check-release-assets: OK"

if [ "$fail_n" -eq 0 ]; then
    echo "RESULT: SELFTEST PASS (${pass_n})"
    exit 0
fi
echo "RESULT: SELFTEST FAIL (${fail_n})"
exit 1
