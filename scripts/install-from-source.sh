#!/usr/bin/env bash
# Network Ultra Server 一键自建脚本(从源码编译)
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
set -euo pipefail

# Pin cwd to /tmp so later `rm -rf $SRC_DIR` doesn't yank the shell's cwd out
# from under us. (Happens when the user happens to be sitting in
# /opt/network-ultra-src when re-running the installer — `git clone` then
# fatals with "Unable to read current working directory".)
cd /tmp

REPO_URL="https://github.com/GeekASMR/network-ultra-server.git"
SRC_DIR="/opt/network-ultra-src"
BIN_PATH="/usr/local/bin/network-ultra-server"
CFG_DIR="/etc/network-ultra"
CFG_FILE="${CFG_DIR}/config.toml"
SVC_FILE="/etc/systemd/system/network-ultra-server.service"
GO_VERSION="1.22.5"

c_red()   { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu()   { printf '\033[36m%s\033[0m\n' "$*"; }
step()    { c_blu "[$1] $2"; }

# 0. 前置检查
if [[ "$(uname -s)" != "Linux" ]]; then
  c_red "本脚本仅支持 Linux,当前系统:$(uname -s)"; exit 1
fi
if [[ "${EUID}" -ne 0 ]]; then
  c_red "请用 root 权限执行(加 sudo)。"; exit 1
fi
if [[ ! -d /run/systemd/system ]]; then
  c_red "需要 systemd(/run/systemd/system 不存在)。"; exit 1
fi

step "1/7" "检测系统架构"
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) c_red "不支持的 CPU 架构:$ARCH"; exit 1 ;;
esac
echo "  架构 = $ARCH (Go 架构 = $GOARCH)"

if ss -tlnp 2>/dev/null | grep -qE ':18900\b'; then
  # If our own systemd service is the one holding the port, gracefully stop
  # it so the rest of the installer can re-deploy. Anything else (different
  # service or container squatting on 18900) is a real conflict and bails.
  if systemctl is-active --quiet network-ultra-server 2>/dev/null \
       && ss -tlnp 2>/dev/null | grep -E ':18900\b' | grep -q 'network-ultra-s'; then
    echo "  端口 18900 被旧版 network-ultra-server 占用,先停止以便升级..."
    systemctl stop network-ultra-server || true
    sleep 1
  else
    c_red "端口 18900 已被其它服务占用,请先停掉冲突服务。"; exit 1
  fi
fi

step "2/7" "确认 Go >= ${GO_VERSION}"
need_go=1
if command -v go >/dev/null 2>&1; then
  cur=$(go version | awk '{print $3}' | sed 's/^go//')
  major=$(echo "$cur" | cut -d. -f1)
  minor=$(echo "$cur" | cut -d. -f2)
  if [[ "$major" -ge 1 && "$minor" -ge 22 ]]; then
    need_go=0
    echo "  已安装 Go $cur"
  else
    echo "  已安装 Go $cur,版本太旧"
  fi
fi
if [[ "$need_go" -eq 1 ]]; then
  echo "  正在下载 Go ${GO_VERSION} 到 /usr/local/go(约 120 MB)..."
  GO_TARBALL="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
  # 国内优先走 mirrors,失败回落官方
  if ! curl -fsSL "https://mirrors.aliyun.com/golang/${GO_TARBALL}" -o "/tmp/${GO_TARBALL}" 2>/dev/null; then
    curl -fsSL "https://go.dev/dl/${GO_TARBALL}" -o "/tmp/${GO_TARBALL}"
  fi
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm -f "/tmp/${GO_TARBALL}"
  export PATH="/usr/local/go/bin:$PATH"
  echo "  Go 安装完成:$(/usr/local/go/bin/go version)"
fi
export PATH="/usr/local/go/bin:$PATH"

step "3/7" "拉取源码"
if [[ -d "$SRC_DIR/.git" ]]; then
  ( cd "$SRC_DIR" && git fetch --tags origin && git reset --hard origin/main )
  echo "  已更新 $SRC_DIR 到最新版本"
else
  rm -rf "$SRC_DIR"
  mkdir -p "$SRC_DIR"
  # Try git clone first (gives us .git for future incremental updates), but
  # fall back to a plain tar.gz from the GitHub release CDN if git is slow
  # or blocked. Many CN ISPs throttle git protocol while letting HTTPS to
  # codeload.github.com pass freely.
  if timeout 30 git clone --depth 1 "$REPO_URL" "$SRC_DIR" 2>/dev/null; then
    echo "  已克隆到 $SRC_DIR (git)"
  else
    echo "  git clone 慢/超时,改走 tar.gz CDN..."
    rm -rf "$SRC_DIR"
    mkdir -p "$SRC_DIR"
    curl -fsSL --max-time 60 \
      "https://github.com/GeekASMR/network-ultra-server/archive/refs/heads/main.tar.gz" \
      -o /tmp/network-ultra-src.tar.gz
    tar xzf /tmp/network-ultra-src.tar.gz -C "$SRC_DIR" --strip-components=1
    rm -f /tmp/network-ultra-src.tar.gz
    echo "  已解压到 $SRC_DIR (tar.gz)"
  fi
fi

step "4/7" "下载依赖并编译"
cd "$SRC_DIR"
# GOPROXY 优先走国内代理,失败回落 direct
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-off}"
export CGO_ENABLED=0

echo "  下载依赖..."
go mod tidy
echo "  编译中..."
go build -trimpath -ldflags="-s -w -X main.buildVersion=$(git rev-parse --short HEAD)" \
  -o "$BIN_PATH" ./cmd/server
echo "  二进制大小 $(ls -lh $BIN_PATH | awk '{print $5}'),已写入 $BIN_PATH"

step "5/7" "生成配置文件"
mkdir -p "$CFG_DIR"
ADMIN_TOKEN=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p -c 64)

# 服务器连接密码(v1.3+)。所有连进来的 VST 客户端都要在 hello 中携带匹配的密码。
# 留空 = 兼容旧客户端、任何人都能连。
# 优先级:
#   1. 命令行环境变量 NU_SERVER_PASSWORD (无人值守安装专用)
#   2. 已有 config.toml 中的现存值 (升级保留)
#   3. 交互输入 (默认随机生成 12 字符)
#   4. 用户按回车跳过 = 公开服务器
EXISTING_PWD=""
if [[ -f "$CFG_FILE" ]]; then
  EXISTING_PWD=$(awk -F\" '/^password[[:space:]]*=/ {print $2; exit}' "$CFG_FILE" 2>/dev/null || echo "")
fi
if [[ -n "${NU_SERVER_PASSWORD:-}" ]]; then
  SERVER_PWD="$NU_SERVER_PASSWORD"
  echo "  已从环境变量读取服务器密码 (NU_SERVER_PASSWORD)"
elif [[ -n "$EXISTING_PWD" ]]; then
  SERVER_PWD="$EXISTING_PWD"
  echo "  保留 $CFG_FILE 中已有的服务器密码"
else
  RAND_PWD=$(openssl rand -base64 9 2>/dev/null | tr -d '/+=' | head -c 12 || head -c 9 /dev/urandom | base64 | tr -d '/+=' | head -c 12)
  if [[ -t 0 ]]; then
    # 交互式终端:让用户决定
    echo ""
    c_blu "  设置服务器密码(只有知道密码的客户端才能连接):"
    echo "    ① 直接回车 = 使用建议的随机密码 [$RAND_PWD]"
    echo "    ② 输入 'open' = 不设密码,任何人都能连"
    echo "    ③ 直接输入你想用的密码"
    read -r -p "  > " USER_INPUT < /dev/tty || USER_INPUT=""
    case "$USER_INPUT" in
      "")     SERVER_PWD="$RAND_PWD" ;;
      "open") SERVER_PWD="" ;;
      *)      SERVER_PWD="$USER_INPUT" ;;
    esac
  else
    # 非交互(管道安装):默认随机密码
    SERVER_PWD="$RAND_PWD"
    echo "  非交互模式,使用随机密码"
  fi
fi

if [[ -f "$CFG_FILE" ]]; then
  echo "  $CFG_FILE 已存在,保留原配置不覆盖。"
else
  cat > "$CFG_FILE" <<EOF
[server]
listen = "0.0.0.0:18900"
health_listen = "127.0.0.1:18901"
max_rooms = 50
max_peers_per_room = 8
max_connections = 200
admin_token = "${ADMIN_TOKEN}"
password = "${SERVER_PWD}"

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
  echo "  已写入 $CFG_FILE"
fi

step "6/7" "注册 systemd 服务"
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
echo "  systemd 服务已启动"

step "7/7" "健康检查"
HEALTH_OK=0
for _ in 1 2 3 4 5; do
  sleep 1
  if curl -fs http://127.0.0.1:18901/healthz > /dev/null 2>&1; then
    HEALTH_OK=1; break
  fi
done
if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "健康检查失败,请查看日志:journalctl -u network-ultra-server -n 50"
  exit 1
fi
echo "  /healthz 响应正常"

PUB_IP=$(curl -fs --max-time 3 https://api.ipify.org 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP=$(curl -fs --max-time 3 https://ifconfig.me 2>/dev/null || echo "")
[[ -z "$PUB_IP" ]] && PUB_IP="<服务器公网 IP>"

cat <<EOF

═════════════════════════════════════════════════════════════
$(c_grn "  Network Ultra Server 已成功启动")
═════════════════════════════════════════════════════════════

  在 VST 插件里填入服务器地址:
    $(c_blu "ws://${PUB_IP}:18900")

EOF
if [[ -n "${SERVER_PWD:-}" ]]; then
cat <<EOF
  $(c_grn "服务器密码(分发给信任的客户端):")
    $(c_blu "${SERVER_PWD}")

  客户端在"服务器密码"栏填入此值才能连接。
  改密码:编辑 $CFG_FILE 的 password = "...",然后 systemctl restart network-ultra-server

EOF
else
cat <<EOF
  $(c_blu "(此服务器未设置密码,任何人都能连接)")
  设置密码:编辑 $CFG_FILE 的 password = "你的密码",然后 systemctl restart network-ultra-server

EOF
fi
cat <<EOF
  Admin Token(已写入 $CFG_FILE):
    ${ADMIN_TOKEN}

  常用命令:
    systemctl status network-ultra-server   # 查看服务状态
    journalctl -u network-ultra-server -f   # 看实时日志
    curl http://127.0.0.1:18901/healthz     # 健康检查
    curl http://127.0.0.1:18901/metrics     # Prometheus 指标

  以后升级:
    再跑一次同样的命令即可
    curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash

  需要 TLS 域名?编辑 $CFG_FILE 的 [tls] 段后重启服务。
  文档:https://github.com/GeekASMR/network-ultra-server

EOF
