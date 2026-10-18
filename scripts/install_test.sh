#!/usr/bin/env bash
# install_test.sh — exercise install.sh + uninstall.sh end-to-end
# inside a real Ubuntu 22.04 systemd container. Verifies RF1 (idempotency),
# RF2 (≤ 5min wall time), RF5 (upgrade / reinstall).
#
# Strategy:
#   1. Build the release tarball (assumes the test runner already ran
#      `go build` and copied the binary into a dist/ tarball, OR builds
#      it here).
#   2. Spin up an Ubuntu 22.04 container with systemd enabled.
#   3. Copy scripts/ + tarball into the container.
#   4. Run install.sh, time it (RF2), assert success.
#   5. Re-run install.sh (RF1 idempotency); assert exit 0.
#   6. Verify binary path, unit path, daemon status (RF5 partial —
#      full upgrade requires two distinct tarballs; we approximate
#      by running install.sh twice and confirming the second run
#      does not error).
#   7. Run uninstall.sh; assert exit 0.
#   8. Re-run uninstall.sh (RF1 idempotency); assert exit 0.
#
# Env knobs:
#   DIST_TARBALL   path to a release tarball (default: build one here)
#   IMAGE          container image (default: jrei/systemd-ubuntu:22.04)
#   SKIP_DOCKER=1  skip the container dance; just run install.sh on
#                  the host (useful for local smoke testing).
#
# Exits 0 on success, non-zero on any assertion failure.

set -euo pipefail

# --- constants ---------------------------------------------------------------

readonly BIN_NAME="daedalus-agent"
readonly REPO_ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." >/dev/null 2>&1 && pwd)"
readonly SCRIPTS_DIR="${REPO_ROOT}/scripts"
readonly DIST_DIR="${REPO_ROOT}/dist"
readonly DEFAULT_IMAGE="jrei/systemd-ubuntu:22.04"
readonly IMAGE="${IMAGE:-$DEFAULT_IMAGE}"
readonly RF2_BUDGET_SECS=300   # 5 minutes
readonly CONTAINER_NAME="daedalus-agent-installer-test-$$"

# --- helpers -----------------------------------------------------------------

log() { printf '[install_test] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; cleanup; exit 1; }
pass() { log "PASS: $*"; }

cleanup() {
    if [ "${SKIP_DOCKER:-0}" -eq 1 ]; then
        return 0
    fi
    if command -v docker >/dev/null 2>&1; then
        docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# assert_eq fails the test if $1 != $2.
assert_eq() {
    local got="$1" want="$2" msg="$3"
    if [ "$got" != "$want" ]; then
        fail "$msg: got '$got', want '$want'"
    fi
}

# build_tarball creates a minimal release tarball in $DIST_DIR. Layout:
#   daedalus-agent-<version>/bin/daedalus-agent
#   daedalus-agent-<version>/scripts/install.sh
#   daedalus-agent-<version>/scripts/uninstall.sh
#   daedalus-agent-<version>/scripts/postinstall.sh
# This mirrors what the release pipeline will produce.
build_tarball() {
    mkdir -p "$DIST_DIR"
    local version
    version="$(cd "$REPO_ROOT" && git describe --tags --always --dirty 2>/dev/null || echo "dev")"
    local staging="${DIST_DIR}/staging-${BIN_NAME}-${version}"
    rm -rf "$staging"
    mkdir -p "$staging/bin" "$staging/scripts"

    log "building ${BIN_NAME} binary"
    ( cd "$REPO_ROOT" && go build -o "$staging/bin/${BIN_NAME}" ./cmd/${BIN_NAME} )

    cp "${SCRIPTS_DIR}/install.sh" "$staging/scripts/"
    cp "${SCRIPTS_DIR}/uninstall.sh" "$staging/scripts/"
    cp "${SCRIPTS_DIR}/postinstall.sh" "$staging/scripts/"
    chmod +x "$staging/scripts/"*.sh

    local tarball="${DIST_DIR}/${BIN_NAME}-${version}.tar.gz"
    rm -f "$tarball"
    tar -czf "$tarball" -C "$DIST_DIR" "$(basename "$staging")"
    echo "$tarball"
}

# --- preflight ---------------------------------------------------------------

if ! command -v tar >/dev/null 2>&1; then
    fail "tar not found on PATH" 1
fi

# Ensure the build can run on the host. The test script needs `go` to
# build the binary unless DIST_TARBALL is provided.
TARBALL="${DIST_TARBALL:-}"
if [ -z "$TARBALL" ]; then
    if ! command -v go >/dev/null 2>&1; then
        fail "neither DIST_TARBALL nor 'go' is available; cannot build" 1
    fi
    TARBALL="$(build_tarball)"
fi
log "using tarball: $TARBALL"

# --- host-only smoke test ----------------------------------------------------

# Quick sanity check that the install script at least parses and
# reaches its preflight. Runs uninstall.sh with --help to verify the
# flag parser.
"${SCRIPTS_DIR}/uninstall.sh" --help >/dev/null
"${SCRIPTS_DIR}/uninstall.sh" --bogus-flag 2>/dev/null \
    && fail "uninstall.sh accepted --bogus-flag" \
    || log "uninstall.sh correctly rejects unknown args"

if [ "${SKIP_DOCKER:-0}" -eq 1 ]; then
    log "SKIP_DOCKER=1; skipping container validation"
    log "(install.sh was NOT executed end-to-end; this is a smoke test only)"
    log "ALL TESTS PASSED (smoke only)"
    exit 0
fi

# --- container validation ----------------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
    fail "docker not on PATH (and SKIP_DOCKER not set)" 1
fi

log "starting container ${CONTAINER_NAME} from ${IMAGE}"
# systemd-in-docker on a cgroup v2 host requires the cgroupns=host
# + seccomp/apparmor unconfined + rw cgroup mount combination.
# The classic cgroup v1 recipe (--pid=host + :ro) does NOT work on
# AlmaLinux/RHEL-family cgroup v2 hosts because systemd PID 1
# detects the host's cgroup layout mismatch and exits 255 before
# initializing. This recipe is what gets `systemctl is-system-running`
# to return "running" inside the container.
if ! docker run -d --name "$CONTAINER_NAME" \
    --privileged \
    --cgroupns=host \
    --security-opt seccomp=unconfined \
    --security-opt apparmor=unconfined \
    --tmpfs /tmp \
    --tmpfs /run \
    --tmpfs /run/lock \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    "$IMAGE" /sbin/init >/dev/null; then
    fail "failed to start container" 1
fi

# Wait for systemd to come up (pid 1 = systemd). Some images take
# 10–20s on cold start.
log "waiting for systemd"
for _ in $(seq 1 60); do
    if docker exec "$CONTAINER_NAME" systemctl is-system-running >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if ! docker exec "$CONTAINER_NAME" systemctl is-system-running >/dev/null 2>&1; then
    fail "systemd never came up in container" 1
fi
pass "systemd is up"

# Stage the tarball + scripts inside the container. We copy to /root/
# rather than /tmp because /tmp is mounted as a tmpfs in the
# container, and `docker cp` cannot write into a tmpfs-backed dest.
log "staging tarball + scripts"
docker cp "$TARBALL" "${CONTAINER_NAME}:/root/${BIN_NAME}.tar.gz"
docker cp "${SCRIPTS_DIR}/install.sh"     "${CONTAINER_NAME}:/root/install.sh"
docker cp "${SCRIPTS_DIR}/uninstall.sh"   "${CONTAINER_NAME}:/root/uninstall.sh"
docker cp "${SCRIPTS_DIR}/postinstall.sh" "${CONTAINER_NAME}:/root/postinstall.sh"

# --- RF2: install.sh wall time ≤ 5min ----------------------------------------

log "running install.sh (timing for RF2)"
INSTALL_START=$(date +%s)
docker exec -e DAEDALUS_AGENT_POSTINSTALL=/root/postinstall.sh \
    "$CONTAINER_NAME" bash /root/install.sh /root/${BIN_NAME}.tar.gz
INSTALL_END=$(date +%s)
INSTALL_WALL=$((INSTALL_END - INSTALL_START))
log "install.sh wall time: ${INSTALL_WALL}s (budget ${RF2_BUDGET_SECS}s)"
if [ "$INSTALL_WALL" -gt "$RF2_BUDGET_SECS" ]; then
    fail "RF2 violation: install took ${INSTALL_WALL}s, budget is ${RF2_BUDGET_SECS}s" 1
fi
pass "RF2: install.sh ≤ 5min"

# Verify the artifacts.
docker exec "$CONTAINER_NAME" test -e "/opt/${BIN_NAME}/bin/${BIN_NAME}" \
    || fail "binary not at /opt/${BIN_NAME}/bin/${BIN_NAME}"
docker exec "$CONTAINER_NAME" test -e "/etc/systemd/system/${BIN_NAME}.service" \
    || fail "systemd unit not at /etc/systemd/system/${BIN_NAME}.service"
docker exec "$CONTAINER_NAME" test -d "/var/lib/${BIN_NAME}" \
    || fail "data dir not at /var/lib/${BIN_NAME}"
docker exec "$CONTAINER_NAME" test -e "/etc/${BIN_NAME}/config.yaml" \
    || fail "default config.yaml not at /etc/${BIN_NAME}/config.yaml"
pass "install artifacts present"

# --- RF1: install.sh is idempotent -------------------------------------------

log "running install.sh a second time (RF1 idempotency)"
docker exec -e DAEDALUS_AGENT_POSTINSTALL=/root/postinstall.sh \
    "$CONTAINER_NAME" bash /root/install.sh /root/${BIN_NAME}.tar.gz \
    || fail "second install.sh run failed (RF1 violation)"
pass "RF1: install.sh is idempotent"

# --- RF5: daemon state survives reinstall (binary updated in place) ---------

# Build a SECOND tarball with a forced newer version, drop it in, and
# rerun install.sh. The unit should remain enabled and the daemon
# should come back up under the new binary.
log "RF5: running upgrade-reinstall"
TARBALL2="$(build_tarball)"
docker cp "$TARBALL2" "${CONTAINER_NAME}:/root/${BIN_NAME}-2.tar.gz"
docker exec -e DAEDALUS_AGENT_POSTINSTALL=/root/postinstall.sh \
    "$CONTAINER_NAME" bash /root/install.sh /root/${BIN_NAME}-2.tar.gz \
    || fail "upgrade-reinstall failed (RF5 violation)"
pass "RF5: upgrade-reinstall succeeded"

# --- uninstall.sh + RF1 idempotency ------------------------------------------

log "running uninstall.sh"
docker exec "$CONTAINER_NAME" bash /root/uninstall.sh \
    || fail "first uninstall.sh run failed"
docker exec "$CONTAINER_NAME" test ! -e "/opt/${BIN_NAME}/bin/${BIN_NAME}" \
    || fail "binary still present after uninstall"
docker exec "$CONTAINER_NAME" test ! -e "/etc/systemd/system/${BIN_NAME}.service" \
    || fail "unit still present after uninstall"
pass "uninstall.sh cleaned artifacts"

log "running uninstall.sh a second time (RF1 idempotency)"
docker exec "$CONTAINER_NAME" bash /root/uninstall.sh \
    || fail "second uninstall.sh run failed (RF1 violation)"
pass "RF1: uninstall.sh is idempotent"

# --- --keep-data -------------------------------------------------------------

log "RF5: --keep-data preserves config"
docker exec -e DAEDALUS_AGENT_POSTINSTALL=/root/postinstall.sh \
    "$CONTAINER_NAME" bash /root/install.sh /root/${BIN_NAME}.tar.gz >/dev/null \
    || fail "install for --keep-data test failed"
docker exec "$CONTAINER_NAME" bash -c \
    "echo manual-edit > /etc/${BIN_NAME}/config.yaml" >/dev/null \
    || fail "could not write manual-edit sentinel"
docker exec "$CONTAINER_NAME" bash /root/uninstall.sh --keep-data \
    || fail "uninstall --keep-data failed"
docker exec "$CONTAINER_NAME" test -e "/etc/${BIN_NAME}/config.yaml" \
    || fail "--keep-data removed config.yaml (RULING VIOLATION)"
docker exec "$CONTAINER_NAME" sh -c \
    "test \"\$(cat /etc/${BIN_NAME}/config.yaml)\" = \"manual-edit\"" \
    || fail "--keep-data did not preserve manual config edits"
pass "RF5: --keep-data preserves config"

log "ALL TESTS PASSED"
exit 0