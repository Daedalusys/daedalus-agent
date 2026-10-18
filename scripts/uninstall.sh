#!/usr/bin/env bash
# uninstall.sh — remove daedalus-agent from a systemd host.
#
# Rulings respected:
#   - RF1: idempotent. Re-runs exit 0; missing units / dirs are fine.
#   - RF5: data preservation is operator-controlled. By default the
#     data dir is removed (clean uninstall). --keep-data preserves
#     /var/lib/daedalus-agent, /var/log/daedalus-agent, and
#     /etc/daedalus-agent so a subsequent install can reuse state.
#
# Args:
#   --keep-data    preserve data/config dirs (default: remove)
#   --purge        alias for the default; explicit and self-documenting
#   -h, --help     print usage
#
# Exits:
#   0   uninstall (or partial uninstall, since no-op steps are fine) succeeded
#   1   bad arguments

set -euo pipefail

# --- constants ---------------------------------------------------------------

readonly BIN_NAME="daedalus-agent"
readonly INSTALL_ROOT="/opt/daedalus-agent"
readonly BIN_PATH="${INSTALL_ROOT}/bin/${BIN_NAME}"
readonly UNIT_PATH="/etc/systemd/system/${BIN_NAME}.service"
readonly UNIT_NAME="${BIN_NAME}.service"
readonly DATA_DIR="/var/lib/${BIN_NAME}"
readonly LOG_DIR="/var/log/${BIN_NAME}"
readonly CONFIG_DIR="/etc/${BIN_NAME}"

# --- args --------------------------------------------------------------------

KEEP_DATA=0

usage() {
    cat <<EOF
usage: uninstall.sh [--keep-data | --purge]

Removes the daedalus-agent binary, systemd unit, and (by default) all
data/config/log directories.

  --keep-data    preserve ${DATA_DIR}, ${LOG_DIR}, and ${CONFIG_DIR}
  --purge        remove data dirs too (this is the default; the flag
                 is provided for symmetry with --keep-data)
  -h, --help     show this help

Exit codes: 0 on success (including re-runs), 1 on bad arguments.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --keep-data) KEEP_DATA=1; shift ;;
        --purge)     KEEP_DATA=0; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)           printf 'uninstall.sh: unknown arg: %s\n' "$1" >&2; usage >&2; exit 1 ;;
    esac
done

# --- helpers -----------------------------------------------------------------

log() { printf '[uninstall] %s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit "${2:-1}"; }

sudo_if_needed() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# best_effort runs cmd; on failure it logs but does not abort. Use for
# cleanup steps that are nice-to-have (e.g. stopping a service that
# might already be stopped).
best_effort() {
    if ! "$@"; then
        log "best-effort step failed (continuing): $*"
    fi
}

# --- preflight ---------------------------------------------------------------

# systemctl is *not* required: an uninstall must succeed even on a
# host where systemd is broken or absent, so long as the file
# artifacts can be removed. We use systemctl only opportunistically.

# --- stop + disable service --------------------------------------------------

if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet "$UNIT_NAME" 2>/dev/null; then
        log "stopping ${UNIT_NAME}"
        best_effort sudo_if_needed systemctl stop "$UNIT_NAME"
    else
        log "no running ${UNIT_NAME} to stop"
    fi

    if systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
        log "disabling ${UNIT_NAME}"
        best_effort sudo_if_needed systemctl disable "$UNIT_NAME"
    else
        log "${UNIT_NAME} not enabled"
    fi
else
    log "systemctl not on PATH; skipping stop/disable"
fi

# --- remove binary -----------------------------------------------------------

if [ -e "$BIN_PATH" ]; then
    log "removing ${BIN_PATH}"
    sudo_if_needed rm -f "$BIN_PATH"
else
    log "binary not present at ${BIN_PATH}"
fi

# Remove the install root if empty (defensive — a half-removed install
# shouldn't leave a stale /opt/daedalus-agent/ dir behind).
if [ -d "$INSTALL_ROOT" ]; then
    if [ -z "$(sudo_if_needed find "$INSTALL_ROOT" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
        log "removing empty ${INSTALL_ROOT}"
        sudo_if_needed rmdir "$INSTALL_ROOT"
    else
        log "${INSTALL_ROOT} not empty (preserved)"
    fi
fi

# --- remove systemd unit -----------------------------------------------------

if [ -e "$UNIT_PATH" ]; then
    log "removing ${UNIT_PATH}"
    sudo_if_needed rm -f "$UNIT_PATH"
else
    log "unit not present at ${UNIT_PATH}"
fi

if command -v systemctl >/dev/null 2>&1; then
    log "reloading systemd"
    best_effort sudo_if_needed systemctl daemon-reload
    # reset-failed clears any "failed" state in systemd's unit tracker.
    # Re-runs and broken prior installs both benefit.
    best_effort sudo_if_needed systemctl reset-failed "$UNIT_NAME" || true
fi

# --- remove data dirs --------------------------------------------------------

if [ "$KEEP_DATA" -eq 1 ]; then
    log "--keep-data: preserving ${DATA_DIR}, ${LOG_DIR}, ${CONFIG_DIR}"
else
    for dir in "$DATA_DIR" "$LOG_DIR" "$CONFIG_DIR"; do
        if [ -e "$dir" ]; then
            log "removing ${dir}"
            sudo_if_needed rm -rf "$dir"
        else
            log "${dir} not present"
        fi
    done
fi

log "uninstall complete"
exit 0