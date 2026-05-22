#!/usr/bin/env bash
# Network Ultra Server - 修改服务器连接密码
#
# 用法:
#   1. 交互式（推荐）:
#        sudo bash set-password.sh
#      脚本会提示你输入新密码（隐藏不回显），自动改 config + 重启服务。
#
#   2. 命令行参数:
#        sudo bash set-password.sh "newpassword"
#
#   3. 关闭密码（公开服务器，任何人可连）:
#        sudo bash set-password.sh --open
#
# 一键远程执行（国内代理）:
#   curl -fsSL https://gh-proxy.com/https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/set-password.sh | sudo bash
#
set -euo pipefail

CFG_FILE="/etc/network-ultra/config.toml"
SERVICE="network-ultra-server"

c_red()   { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu()   { printf '\033[36m%s\033[0m\n' "$*"; }

# 0. 前置检查
if [[ "${EUID}" -ne 0 ]]; then
  c_red "请用 root 权限执行(加 sudo)。"; exit 1
fi
if [[ ! -f "$CFG_FILE" ]]; then
  c_red "未找到 $CFG_FILE。请先跑 install-from-source.sh 完成初次安装。"
  exit 1
fi

# 1. 解析新密码来源（参数 / 标准输入 / 交互）
NEW_PWD=""
if [[ $# -ge 1 ]]; then
  case "$1" in
    --open|-o|"")
      NEW_PWD=""
      c_blu "将关闭密码保护(公开服务器)"
      ;;
    --help|-h)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *)
      NEW_PWD="$1"
      ;;
  esac
else
  # 没参数 → 交互式输入。隐藏回显避免肩窥。
  if [[ ! -t 0 ]]; then
    c_red "未通过参数提供密码,且当前不是交互式终端(可能 stdin 来自 pipe)。"
    c_red "请改用: sudo bash set-password.sh \"新密码\"   或   sudo bash set-password.sh --open"
    exit 1
  fi
  echo ""
  c_blu "修改 Network Ultra 服务器密码"
  echo "  ① 直接回车 = 关闭密码(公开服务器,任何人可连)"
  echo "  ② 输入新密码后回车"
  echo ""
  read -r -s -p "  新密码: " NEW_PWD
  echo ""
  if [[ -n "$NEW_PWD" ]]; then
    read -r -s -p "  再输一次确认: " CONFIRM
    echo ""
    if [[ "$NEW_PWD" != "$CONFIRM" ]]; then
      c_red "两次输入不一致,已取消。"
      exit 1
    fi
  fi
fi

# 2. 改写 config.toml 的 password 行
#    用 awk 而不是 sed 是为了正确处理:
#      - password 含特殊字符($ \ / 等)
#      - [server] 段可能不存在 password 行(老 config 升级场景)
#      - 不动其它键
TMP_CFG="$(mktemp)"
trap 'rm -f "$TMP_CFG"' EXIT

awk -v new_pwd="$NEW_PWD" '
  BEGIN { in_server = 0; replaced = 0 }
  /^\[server\][[:space:]]*$/ { in_server = 1; print; next }
  /^\[/ {
    # 离开 [server] 段;若一直没遇到 password 行就在此处补一行
    if (in_server == 1 && replaced == 0) {
      print "password = \"" new_pwd "\""
      replaced = 1
    }
    in_server = 0
    print; next
  }
  {
    if (in_server == 1 && $0 ~ /^[[:space:]]*password[[:space:]]*=/) {
      print "password = \"" new_pwd "\""
      replaced = 1
      next
    }
    print
  }
  END {
    # 文件结束时若 [server] 还活着且没替换过(末尾段)
    if (in_server == 1 && replaced == 0) {
      print "password = \"" new_pwd "\""
    }
  }
' "$CFG_FILE" > "$TMP_CFG"

# 3. 校验:确认我们真的写了一行 password 进去
if ! grep -q '^password[[:space:]]*=' "$TMP_CFG"; then
  c_red "改写失败,$TMP_CFG 中未发现 password 行。原 config 未动。"
  exit 1
fi

# 4. 备份并替换
cp -p "$CFG_FILE" "${CFG_FILE}.bak.$(date +%Y%m%d_%H%M%S)"
mv "$TMP_CFG" "$CFG_FILE"
chmod 0640 "$CFG_FILE"
trap - EXIT

# 5. 重启服务
echo ""
c_blu "重启 ${SERVICE}..."
systemctl restart "$SERVICE"
sleep 1

# 6. 健康检查 + 状态确认
HEALTH_OK=0
for _ in 1 2 3 4 5; do
  if curl -fs http://127.0.0.1:18901/healthz > /dev/null 2>&1; then
    HEALTH_OK=1; break
  fi
  sleep 1
done

if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "重启后健康检查失败,请查看:journalctl -u ${SERVICE} -n 50"
  exit 1
fi

# 看 systemd 日志最后一条 password gating 状态
GATING_LINE=$(journalctl -u "$SERVICE" --since "30 seconds ago" --no-pager -o cat 2>/dev/null \
              | grep -F "password gating" | tail -1 || true)

echo ""
c_grn "✓ 服务已重启,运行正常"
if [[ -n "$NEW_PWD" ]]; then
  echo ""
  c_blu "新密码已生效:"
  echo "    $NEW_PWD"
  echo ""
  echo "  请把这个密码分发给信任的客户端使用者。"
  echo "  客户端在\"服务器密码\"栏填入此值才能连接。"
else
  echo ""
  c_blu "密码保护已关闭(公开服务器,任何人可连)"
fi
if [[ -n "$GATING_LINE" ]]; then
  echo ""
  echo "  服务端日志确认: $GATING_LINE"
fi
