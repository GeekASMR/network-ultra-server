#!/usr/bin/env bash
# Network Ultra Server one-shot installer for Linux.
# Usage:
#   curl -fsSL https://github.com/GeekASMR/network-ultra-server/releases/latest/download/install.sh | sudo bash
set -euo pipefail

REPO="GeekASMR/network-ultra-server"
BIN_PATH="/usr/local/bin/network-ultra-server"
CFG_DIR="/etc/network-ultra"
CFG_FILE="${CFG_DIR}/config.toml"
SVC_FILE="/etc/systemd/system/network-ultra-server.service"

c_red()   { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu()   { printf '\033[36m%s\033[0m\n' "$*"; }
step()    { c_blu "[$1] $2"; }

# 0. Sanity checks
if [[ "$(uname -s)" != "Linux" ]]; then
  c_red "This installer is Linux-only. Got: $(uname -s)"
  exit 1
fi
if [[ "${EUID}" -ne 0 ]]; then
  c_red "Please run as root (use sudo)."
  exit 1
fi
if [[ ! -d /run/systemd/system ]]; then
  c_red "systemd is required (no /run/systemd/system found)."
  exit 1
fi

step "1/6" "Detecting platform"
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  TARGET="linux-amd64" ;;
  aarch64) TARGET="linux-arm64" ;;
  *)       c_red "Unsupported arch: $ARCH"; exit 1 ;;
esac
echo "  arch=$ARCH target=$TARGET"

if ss -tlnp 2>/dev/null | grep -qE ':18900\b'; then
  c_red "Port 18900 already in use. Free it first or stop conflicting services."
  exit 1
fi

step "2/6" "Downloading binary"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

URL_BIN="https://github.com/${REPO}/releases/latest/download/network-ultra-server-${TARGET}"
URL_SUM="${URL_BIN}.sha256"

curl -fsSL "$URL_BIN" -o "$TMP/nus.bin"
curl -fsSL "$URL_SUM" -o "$TMP/nus.sha256" || true
if [[ -s "$TMP/nus.sha256" ]]; then
  ( cd "$TMP" && sha256sum -c <(awk '{print $1"  nus.bin"}' nus.sha256) ) || { c_red "Checksum failed"; exit 1; }
fi
install -m 0755 "$TMP/nus.bin" "$BIN_PATH"
echo "  installed to $BIN_PATH"

step "3/6" "Generating config"
mkdir -p "$CFG_DIR"
ADMIN_TOKEN=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p -c 64)
if [[ -f "$CFG_FILE" ]]; then
  echo "  $CFG_FILE already exists; keeping existing config."
else
  cat > "$CFG_FILE" <<EOF
[server]
listen = "0.0.0.0:18900"
health_listen = "127.0.0.1:18901"
max_rooms = 50
max_peers_per_room = 8
max_connections = 200
admin_token = "${ADMIN_TOKEN}"

[tls]
enabled = false
cert_file = ""
key_file = ""
auto_letsencrypt = false
domain = ""
email = ""

[log]
level = "info"
format = "json"
path = ""

[ratelimit]
hello_per_ip_per_minute = 10
room_create_per_peer_per_minute = 5
room_join_per_peer_per_minute = 30
room_list_per_peer_per_minute = 60
audio_frames_per_peer_per_second = 200
EOF
  chmod 0640 "$CFG_FILE"
  echo "  wrote $CFG_FILE"
fi

step "4/6" "Installing systemd service"
cat > "$SVC_FILE" <<'EOF'
[Unit]
Description=Network Ultra Audio Server
Documentation=https://github.com/GeekASMR/network-ultra-server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/network-ultra-server -config /etc/network-ultra/config.toml
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/network-ultra
ReadOnlyPaths=/etc/network-ultra
StateDirectory=network-ultra
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now network-ultra-server >/dev/null
echo "  systemctl unit installed and started"

step "5/6" "Health check"
HEALTH_OK=0
for _ in 1 2 3 4 5; do
  sleep 1
  if curl -fs http://127.0.0.1:18901/healthz > /dev/null 2>&1; then
    HEALTH_OK=1
    break
  fi
done
if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "Health check failed. Check: journalctl -u network-ultra-server -n 50"
  exit 1
fi
echo "  /healthz responding OK"

step "6/6" "Connection info"
PUB_IP=$(curl -fs --max-time 3 https://api.ipify.org 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP=$(curl -fs --max-time 3 https://ifconfig.me 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP="<your-server-ip>"

cat <<EOF

═════════════════════════════════════════════════════════════
$(c_grn "  Network Ultra Server is running")
═════════════════════════════════════════════════════════════

  Connect from your VST plugin:
    $(c_blu "ws://${PUB_IP}:18900")

  Admin token (write this down, it is also in $CFG_FILE):
    ${ADMIN_TOKEN}

  Useful commands:
    systemctl status network-ultra-server
    journalctl -u network-ultra-server -f
    curl http://127.0.0.1:18901/healthz
    curl http://127.0.0.1:18901/metrics

  Need TLS / a domain? Edit $CFG_FILE and restart the service.
  Docs:  https://github.com/${REPO}

EOF
