#!/usr/bin/env bash
# Network Ultra Server source-install for Linux (no release binary required).
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
set -euo pipefail

REPO_URL="https://github.com/GeekASMR/network-ultra-server.git"
SRC_DIR="/opt/network-ultra-src"
BIN_PATH="/usr/local/bin/network-ultra-server"
CFG_DIR="/etc/network-ultra"
CFG_FILE="${CFG_DIR}/config.toml"
SVC_FILE="/etc/systemd/system/network-ultra-server.service"
GO_MIN="1.22"

c_red()   { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu()   { printf '\033[36m%s\033[0m\n' "$*"; }
step()    { c_blu "[$1] $2"; }

# 0. Sanity checks
if [[ "$(uname -s)" != "Linux" ]]; then
  c_red "Linux only. Got: $(uname -s)"; exit 1
fi
if [[ "${EUID}" -ne 0 ]]; then
  c_red "Run as root (sudo)."; exit 1
fi
if [[ ! -d /run/systemd/system ]]; then
  c_red "systemd required."; exit 1
fi

step "1/7" "Detect platform"
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) c_red "Unsupported arch: $ARCH"; exit 1 ;;
esac
echo "  arch=$ARCH (goarch=$GOARCH)"

if ss -tlnp 2>/dev/null | grep -qE ':18900\b'; then
  c_red "Port 18900 already in use."; exit 1
fi

step "2/7" "Ensure Go >= ${GO_MIN}"
need_go=1
if command -v go >/dev/null 2>&1; then
  cur=$(go version | awk '{print $3}' | sed 's/^go//')
  major=$(echo "$cur" | cut -d. -f1)
  minor=$(echo "$cur" | cut -d. -f2)
  want_minor=22
  if [[ "$major" -ge 1 && "$minor" -ge "$want_minor" ]]; then
    need_go=0
    echo "  found go $cur"
  else
    echo "  found go $cur, too old"
  fi
fi
if [[ "$need_go" -eq 1 ]]; then
  echo "  installing Go 1.22.5 to /usr/local/go ..."
  GO_TARBALL="go1.22.5.linux-${GOARCH}.tar.gz"
  curl -fsSL "https://go.dev/dl/${GO_TARBALL}" -o "/tmp/${GO_TARBALL}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm -f "/tmp/${GO_TARBALL}"
  export PATH="/usr/local/go/bin:$PATH"
  echo "  go installed: $(/usr/local/go/bin/go version)"
fi
export PATH="/usr/local/go/bin:$PATH"

step "3/7" "Fetch source"
if [[ -d "$SRC_DIR/.git" ]]; then
  ( cd "$SRC_DIR" && git fetch --tags origin && git reset --hard origin/main )
else
  rm -rf "$SRC_DIR"
  git clone --depth 1 "$REPO_URL" "$SRC_DIR"
fi
echo "  source at $SRC_DIR"

step "4/7" "Build"
cd "$SRC_DIR"
GOPROXY=${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct} \
GOSUMDB=${GOSUMDB:-off} \
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$(git rev-parse --short HEAD)" \
  -o "$BIN_PATH" ./cmd/server
echo "  built $(ls -lh $BIN_PATH | awk '{print $5}') → $BIN_PATH"

step "5/7" "Generate config"
mkdir -p "$CFG_DIR"
ADMIN_TOKEN=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p -c 64)
if [[ -f "$CFG_FILE" ]]; then
  echo "  $CFG_FILE exists; keeping."
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

step "6/7" "Install systemd unit"
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
echo "  systemctl unit active"

step "7/7" "Health check"
HEALTH_OK=0
for _ in 1 2 3 4 5; do
  sleep 1
  if curl -fs http://127.0.0.1:18901/healthz > /dev/null 2>&1; then
    HEALTH_OK=1; break
  fi
done
if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "Health check failed. Check: journalctl -u network-ultra-server -n 50"
  exit 1
fi
echo "  /healthz responding OK"

PUB_IP=$(curl -fs --max-time 3 https://api.ipify.org 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP=$(curl -fs --max-time 3 https://ifconfig.me 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP="<your-server-ip>"

cat <<EOF

═════════════════════════════════════════════════════════════
$(c_grn "  Network Ultra Server is running")
═════════════════════════════════════════════════════════════

  Connect from your VST plugin:
    $(c_blu "ws://${PUB_IP}:18900")

  Admin token (in $CFG_FILE):
    ${ADMIN_TOKEN}

  Useful commands:
    systemctl status network-ultra-server
    journalctl -u network-ultra-server -f
    curl http://127.0.0.1:18901/healthz

  Update later:
    curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash

EOF
