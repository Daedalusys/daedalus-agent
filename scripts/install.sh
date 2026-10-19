#!/usr/bin/env bash
# install.sh — install or upgrade daedalus-agent from a release tarball.
#
# Rulings respected (from the plan author):
#   - RF1: idempotent. Re-runs do not choke on existing files, env
#     diffs, or a running old daemon. Exits 0 on success.
#   - RF2: ≤ 5min wall time on a clean Ubuntu 22.04 container.
#   - RF5: upgrades stop the old daemon before the new binary takes
#     over; old systemd unit is overwritten cleanly.
#
# Idempotency strategy (each step survives a prior partial run):
#   - Stop+disable best-effort: missing service is fine, errors are
#     collected but do not abort the install.
#   - Binary dir created with -p; old binary removed before copy.
#   - Systemd unit written from a single heredoc (idempotent).
#   - Postinstall is re-runnable (it skips work already done).
#
# Args:
#   $1  tarball path (default: dist/daedalus-agent-*.tar.gz, first match)
#
# Exits:
#   0   install (or upgrade) succeeded
#   1   bad arguments / tarball not found / extraction failed
#   2   systemd unavailable (systemctl not on PATH)

set -euo pipefail

# --- constants ---------------------------------------------------------------

readonly BIN_NAME="daedalus-agent"
readonly INSTALL_ROOT="/opt/daedalus-agent"
readonly BIN_PATH="${INSTALL_ROOT}/bin/${BIN_NAME}"
readonly UNIT_PATH="/etc/systemd/system/${BIN_NAME}.service"
readonly UNIT_NAME="${BIN_NAME}.service"

# Systemd unit body. Single-sourced from install.sh so that the path
# constants above and the ExecStart below cannot drift.
readonly UNIT_BODY="[Unit]
Description=daedalus-agent: heterogeneous Linux endpoint for daedalus fleet control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=${BIN_PATH} start --config /etc/daedalus-agent/config.yaml
KillSignal=SIGTERM
TimeoutStopSec=35
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target"

# --- helpers -----------------------------------------------------------------

log() { printf '[install] %s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit "${2:-1}"; }

# sudo_if_needed runs cmd under sudo when EUID is non-zero. The
# systemd unit and binary paths all require root; if the operator
# already ran install.sh as root, this is a no-op.
sudo_if_needed() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# script_dir resolves the directory holding this script, even when
# the script is invoked through a symlink (common in /usr/local/bin
# distros).
script_dir() {
    local src="${BASH_SOURCE[0]}"
    while [ -L "$src" ]; do
        local dir
        dir="$(cd -P "$(dirname "$src")" >/dev/null 2>&1 && pwd)"
        src="$(readlink "$src")"
        [[ "$src" != /* ]] && src="${dir}/${src}"
    done
    cd -P "$(dirname "$src")" >/dev/null 2>&1 && pwd
}

# --- preflight ---------------------------------------------------------------

SCRIPT_DIR="$(script_dir)"
# Allow the test harness to swap postinstall.sh for a stub; otherwise
# the script beside install.sh wins.
POSTINSTALL="${DAEDALUS_AGENT_POSTINSTALL:-${SCRIPT_DIR}/postinstall.sh}"

if ! command -v systemctl >/dev/null 2>&1; then
    fail "systemctl not found on PATH — this installer targets systemd hosts (Ubuntu 22.04+, etc.)" 2
fi

# Locate the tarball.
TARBALL="${1:-}"
if [ -z "$TARBALL" ]; then
    # Pick the first matching tarball, sorted by mtime descending, so
    # a fresh build naturally wins.
    shopt -s nullglob
    candidates=("${SCRIPT_DIR}/../dist/${BIN_NAME}-"*.tar.gz)
    shopt -u nullglob
    if [ "${#candidates[@]}" -eq 0 ]; then
        fail "no tarball supplied and none found in dist/${BIN_NAME}-*.tar.gz" 1
    fi
    TARBALL="$(ls -1t "${candidates[@]}" | head -n 1)"
fi

if [ ! -r "$TARBALL" ]; then
    fail "tarball not readable: $TARBALL" 1
fi

log "installing from $TARBALL"

# --- extract -----------------------------------------------------------------

# WORK must be on a non-noexec mount. /tmp is tmpfs-mounted with
# noexec on systemd-managed hosts (including the jrei/systemd-ubuntu
# container we validate in), so the kernel refuses to execute the
# extracted binary even when its mode bits say 0755. /var/tmp is the
# conventional non-noexec temp dir on every mainstream distro.
WORK="$(mktemp -d -p /var/tmp -t daedalus-agent-install.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

if ! tar -xzf "$TARBALL" -C "$WORK"; then
    fail "failed to extract $TARBALL" 1
fi

# Convention: tarballs ship a single top-level directory
# (${BIN_NAME}-<version>/) containing bin/${BIN_NAME} and a copy of
# scripts/. We accept three layouts:
#   1. WORK/bin/${BIN_NAME}                 (flat tarball)
#   2. WORK/${BIN_NAME}/bin/${BIN_NAME}    (no-version dir)
#   3. WORK/<anything>/bin/${BIN_NAME}      (version-suffixed dir)
# Layout 3 covers real release tarballs (${BIN_NAME}-<version>/).
# We auto-discover by looking for a directory that contains bin/${BIN_NAME}.
if [ -d "${WORK}/bin" ] && [ -f "${WORK}/bin/${BIN_NAME}" ]; then
    BIN_SRC="${WORK}/bin/${BIN_NAME}"
elif [ -d "${WORK}/${BIN_NAME}/bin/${BIN_NAME}" ]; then
    BIN_SRC="${WORK}/${BIN_NAME}/bin/${BIN_NAME}"
else
    # Find the first subdir of WORK that contains bin/${BIN_NAME}.
    BIN_SRC=""
    for candidate in "${WORK}"/*/; do
        [ -d "$candidate" ] || continue
        if [ -f "${candidate}bin/${BIN_NAME}" ]; then
            BIN_SRC="${candidate}bin/${BIN_NAME}"
            break
        fi
    done
    if [ -z "$BIN_SRC" ]; then
        fail "tarball layout unrecognized: expected bin/${BIN_NAME} somewhere under $WORK" 1
    fi
fi

if [ ! -x "$BIN_SRC" ]; then
    fail "extracted binary is not executable: $BIN_SRC" 1
fi

# --- stop old daemon (best-effort) -------------------------------------------

if systemctl is-active --quiet "$UNIT_NAME" 2>/dev/null; then
    log "stopping existing ${UNIT_NAME}"
    sudo_if_needed systemctl stop "$UNIT_NAME" || log "stop failed (continuing)"
else
    log "no running ${UNIT_NAME} to stop"
fi

# --- install binary ----------------------------------------------------------

log "installing binary to ${BIN_PATH}"
sudo_if_needed mkdir -p "${INSTALL_ROOT}/bin"
# Remove any stale binary first; a partial install (file in place but
# not executable, or wrong-arch) is the common case RF5 guards against.
sudo_if_needed rm -f "${BIN_PATH}"
if ! sudo_if_needed cp "$BIN_SRC" "${BIN_PATH}"; then
    fail "failed to copy binary to ${BIN_PATH}" 1
fi
sudo_if_needed chmod 0755 "${BIN_PATH}"

# --- write systemd unit ------------------------------------------------------

log "writing unit to ${UNIT_PATH}"
# Overwrite atomically via a temp file in the same dir (same filesystem
# → guaranteed atomic rename on Linux).
UNIT_TMP="${UNIT_PATH}.tmp.$$"
printf '%s\n' "$UNIT_BODY" | sudo_if_needed tee "$UNIT_TMP" >/dev/null
sudo_if_needed chmod 0644 "$UNIT_TMP"
sudo_if_needed mv -f "$UNIT_TMP" "$UNIT_PATH"

# --- postinstall (data dirs + default config + README) -----------------------

if [ -x "$POSTINSTALL" ]; then
    log "running postinstall: $POSTINSTALL"
    if ! "$POSTINSTALL"; then
        fail "postinstall failed: $POSTINSTALL" 1
    fi
else
    log "postinstall script missing or not executable ($POSTINSTALL); skipping"
fi

# --- activate service --------------------------------------------------------

log "reloading systemd"
sudo_if_needed systemctl daemon-reload

log "enabling + starting ${UNIT_NAME}"
# enable --now is idempotent: a previously-enabled unit just gets
# re-started; a never-enabled one gets enabled and started.
sudo_if_needed systemctl enable --now "$UNIT_NAME"

log "install complete: ${BIN_PATH}"
exit 0