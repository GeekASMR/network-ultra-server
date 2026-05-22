#!/usr/bin/env bash
#
# 服务端一键升级脚本 — 拉最新代码、重新编译、平滑重启 systemd 服务。
#
# 用法（在服务器上执行，不要在本地运行）:
#   curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/update-server.sh | sudo bash
# 或者：
#   ssh root@你的服务器IP 'curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/update-server.sh | sudo bash'
#
# 前提：
#   * 服务器已经通过 install-from-source.sh 完成首次安装
#   * /opt/network-ultra-src 是 git 仓库（首次安装会创建）
#   * /etc/systemd/system/network-ultra-server.service 已注册
#
# 这个脚本只做"升级"，不重新写配置文件、不改防火墙、不重置 admin token。

set -euo pipefail

# 切到稳定 cwd 防止脚本运行中目录被 git 操作影响
cd /tmp

SRC_DIR="/opt/network-ultra-src"
BIN_PATH="/usr/local/bin/network-ultra-server"
SVC_NAME="network-ultra-server"
GO_PATH="/usr/local/go/bin/go"

c_red() { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn() { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu() { printf '\033[36m%s\033[0m\n' "$*"; }
step()  { c_blu "[$1] $2"; }

if [[ "${EUID}" -ne 0 ]]; then
  c_red "请用 root 权限执行（加 sudo）。"; exit 1
fi

if [[ ! -d "$SRC_DIR/.git" ]]; then
  c_red "找不到 $SRC_DIR/.git — 这台服务器看起来还没装过。请先跑 install-from-source.sh。"
  exit 1
fi

if [[ ! -x "$GO_PATH" ]] && ! command -v go >/dev/null 2>&1; then
  c_red "找不到 go 编译器。请先跑 install-from-source.sh 装好 Go。"
  exit 1
fi
[[ -x "$GO_PATH" ]] || GO_PATH="$(command -v go)"

step "1/5" "拉取最新代码"
cd "$SRC_DIR"
# fetch+reset 而不是 pull：避免本地修改（如果有的话）阻塞升级
git fetch --tags origin
OLD_REV=$(git rev-parse --short HEAD || echo "unknown")
git reset --hard origin/main
NEW_REV=$(git rev-parse --short HEAD)
echo "  $OLD_REV → $NEW_REV"

if [[ "$OLD_REV" == "$NEW_REV" ]]; then
  echo "  代码已是最新，无需升级。"
  echo "  （如果你想强制重新编译并重启，可以先 systemctl restart $SVC_NAME）"
  exit 0
fi

step "2/5" "编译"
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-off}"
export CGO_ENABLED=0

# 临时输出到 .new，编译成功才替换正在跑的二进制，避免 crash 半成品
TMP_BIN="${BIN_PATH}.new"
"$GO_PATH" build -trimpath \
  -ldflags="-s -w -X main.buildVersion=$NEW_REV" \
  -o "$TMP_BIN" ./cmd/server
echo "  编译完成: $(ls -lh $TMP_BIN | awk '{print $5}')"

step "3/5" "停止旧服务"
systemctl stop "$SVC_NAME"

step "4/5" "替换二进制"
mv -f "$TMP_BIN" "$BIN_PATH"
chmod +x "$BIN_PATH"

step "5/5" "启动新服务并健康检查"
# UDP 数据面新增 18902 端口（v1.2+）。如果 iptables 默认 DROP，需要打开。
# 用 iptables 而不是 ufw 因为腾讯云轻量很多模板没装 ufw，iptables 一定有。
# 同步开 INPUT 和 OUTPUT，规则去重避免重复 append。
if command -v iptables >/dev/null 2>&1; then
  if ! iptables -C INPUT -p udp --dport 18902 -j ACCEPT 2>/dev/null; then
    iptables -A INPUT -p udp --dport 18902 -j ACCEPT || true
    echo "  已开放 UDP 18902（INPUT）"
  fi
  if ! iptables -C OUTPUT -p udp --sport 18902 -j ACCEPT 2>/dev/null; then
    iptables -A OUTPUT -p udp --sport 18902 -j ACCEPT || true
    echo "  已开放 UDP 18902（OUTPUT）"
  fi
fi

# 如果 config.toml 里没有 udp_advertise_host，加上 — 否则服务器会下发
# 0.0.0.0:18902 给客户端，客户端无法连。用本机第一个外网 IP 作默认值。
CONF=/etc/network-ultra/config.toml
if [[ -f "$CONF" ]] && ! grep -q '^udp_advertise_host' "$CONF"; then
  PUBLIC_IP="$(hostname -I | awk '{print $1}')"
  if [[ -n "$PUBLIC_IP" ]]; then
    # 在 [server] section 末尾追加。如果没 [server] 段就什么都不动；
    # 这种情况说明用户用了非默认 config，让他自己加。
    awk -v ip="$PUBLIC_IP" '
      /^\[server\]/ { in_server=1 }
      /^\[/ && !/^\[server\]/ && in_server {
        print "udp_advertise_host = \"" ip "\""
        in_server=0
      }
      { print }
      END {
        if (in_server) print "udp_advertise_host = \"" ip "\""
      }
    ' "$CONF" > "$CONF.new" && mv "$CONF.new" "$CONF"
    echo "  config.toml 已加 udp_advertise_host = \"$PUBLIC_IP\""
  fi
fi

systemctl start "$SVC_NAME"

# 给服务 3 秒钟起来
HEALTH_OK=0
for i in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  if curl -fs http://127.0.0.1:18901/healthz > /dev/null 2>&1; then
    HEALTH_OK=1; break
  fi
done

if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "健康检查失败！服务可能没起来，请查看日志:"
  c_red "  journalctl -u $SVC_NAME -n 50 --no-pager"
  exit 1
fi

cat <<EOF

═════════════════════════════════════════════════════════════
$(c_grn "  ✓ Network Ultra Server 升级完成")
═════════════════════════════════════════════════════════════

  版本:  $OLD_REV → $NEW_REV
  状态:  $(systemctl is-active $SVC_NAME)
  端点:  ws://$(hostname -I | awk '{print $1}'):18900
  UDP :  $(hostname -I | awk '{print $1}'):18902 (新音频通道, 自动 fallback 到 ws)

  实时日志:
    journalctl -u $SVC_NAME -f

  健康状态:
    curl http://127.0.0.1:18901/healthz
EOF
