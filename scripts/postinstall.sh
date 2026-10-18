#!/usr/bin/env bash
# postinstall.sh — create daedalus-agent's runtime directories and
# default config. Called by install.sh after the binary is in place.
#
# Rulings respected:
#   - RF1: idempotent. Each step uses -p / "skip if exists" semantics
#     so a re-run is a no-op.
#
# What this creates:
#   /var/lib/daedalus-agent/{audit,cache,state}/  0755 root:root
#   /var/log/daedalus-agent/                     0755 root:root
#   /etc/daedalus-agent/                         0755 root:root
#       ├── README.md    (one-line "do not edit while running")
#       └── config.yaml  (default Config values; only written if absent)
#
# Args: none.
# Exits: 0 always (postinstall is best-effort cleanup; the install
#        should not fail because a dir couldn't be created).

set -euo pipefail

# --- constants ---------------------------------------------------------------

readonly BIN_NAME="daedalus-agent"
readonly DATA_DIR="/var/lib/${BIN_NAME}"
readonly LOG_DIR="/var/log/${BIN_NAME}"
readonly CONFIG_DIR="/etc/${BIN_NAME}"

# Default config.yaml body. Mirrors config.Default() in Go; if either
# drifts the daemon will log a warning at startup. Keep this in sync.
readonly DEFAULT_CONFIG="agent_id: agent-local
control_plane_addr: https://control.daedalusys.local:7443
cert_path: /etc/daedalus-agent/tls/agent.pem
key_path: /etc/daedalus-agent/tls/agent.key
ca_path: /etc/daedalus-agent/tls/ca.pem
data_dir: /var/lib/daedalus-agent
audit_dir: /var/log/daedalus-agent
"

# README body. One line is enough — operators only need to know not to
# edit the file while the daemon is running.
readonly README_BODY="agent config dir — do not edit while the daemon is running
"

# --- helpers -----------------------------------------------------------------

log() { printf '[postinstall] %s\n' "$*" >&2; }

sudo_if_needed() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# write_if_absent creates path with given content only if path does
# not yet exist. Used for the default config.yaml so a manual edit is
# not clobbered by a reinstall.
write_if_absent() {
    local path="$1"
    local content="$2"
    local mode="$3"
    if [ -e "$path" ]; then
        log "preserving existing ${path}"
        return 0
    fi
    local tmp
    tmp="$(mktemp -t daedalus-agent-postinstall.XXXXXX)"
    # shellcheck disable=SC2064  # we want tmp captured NOW, not at trap time
    trap "rm -f '$tmp'" RETURN
    printf '%s' "$content" > "$tmp"
    sudo_if_needed install -d -m 0755 "$(dirname "$path")"
    sudo_if_needed install -m "$mode" "$tmp" "$path"
    rm -f "$tmp"
    log "wrote ${path}"
}

# --- create runtime dirs -----------------------------------------------------

log "creating runtime directories"
sudo_if_needed install -d -m 0755 \
    "${DATA_DIR}/audit" \
    "${DATA_DIR}/cache" \
    "${DATA_DIR}/state" \
    "${LOG_DIR}" \
    "${CONFIG_DIR}"

# --- default config + README -------------------------------------------------

write_if_absent "${CONFIG_DIR}/config.yaml" "$DEFAULT_CONFIG" 0600
write_if_absent "${CONFIG_DIR}/README.md"   "$README_BODY"    0644

log "postinstall complete"
exit 0