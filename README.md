# Network Ultra Server

[Network Ultra](https://github.com/GeekASMR/network-ultra-server/releases/latest) VST3 插件的中心转发服务器。让两个或多个异地音乐人在各自 DAW 中通过网络协作合奏。

```
┌──────────┐         ┌──────────────┐          ┌──────────┐
│ DAW (A)  │  ws://  │ Network      │  ws://   │  DAW (B) │
│ Network  ├────────►│ Ultra Server ├─────────►│ Network  │
│ Ultra    │◄────────┤ (this repo)  │◄─────────┤ Ultra    │
│ VST3     │         │              │          │ VST3     │
└──────────┘         └──────────────┘          └──────────┘
```

服务器是纯转发层（不解码音频、不存储数据），只负责房间编排 + 二进制帧 fan-out。客户端见 [Releases](https://github.com/GeekASMR/network-ultra-server/releases/latest)。

## 2 分钟自建（小白版）

### 准备

- 一台 Linux 服务器（Ubuntu / Debian / CentOS / Alibaba / Tencent 都行）
- 至少 1 核 1G 内存，30 Mbps 上传带宽够 2~3 人合奏
- 服务器配置页面把 **入站 18900 (TCP) 和 18902 (UDP) 端口** 都放开（云防火墙/安全组）
  - TCP 18900：控制信令 + 音频兜底通道
  - UDP 18902：音频主通道（v1.2+ 加入，避免 TCP head-of-line 阻塞）

### 一键安装

SSH 登录服务器（root），复制粘贴这一条：

```bash
curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
```

脚本自动：检测架构 → 装 Go → 拉源码 → 编译 → 生成配置 → 注册 systemd → 启动 → 健康检查。约 1~3 分钟。

成功的标志：屏幕底部出现绿色 `Network Ultra Server 已成功启动` + 服务器公网 IP 和 admin token。

### 验证从外网能连

在你自己电脑（不是服务器上）打开 PowerShell 或终端：

```powershell
# Windows PowerShell
Test-NetConnection -ComputerName <你的服务器IP> -Port 18900
# 看 TcpTestSucceeded 是否为 True
```

```bash
# Linux / macOS
nc -zv <你的服务器IP> 18900
# 看是否输出 "succeeded"
```

如果不通，看下面 [常见坑](#常见坑) 排查。

### 在 VST 插件里使用

打开 DAW，挂载 Network Ultra 插件，服务器地址填：

```
ws://<你的服务器IP>:18900
```

输入用户名 → 连接 → 创建/加入房间。

---

## 常见坑

### Q1：脚本卡在 `git clone` 报 `fatal: Unable to read current working directory`

**原因**：你 SSH 进来后正好 cd 到了 `/opt/network-ultra-src`，脚本第三步 rm 这个目录后当前 shell 失去 cwd。

**解决**：
```bash
cd /tmp
curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
```

最新版脚本已修，但这个习惯始终保留更稳。

### Q2：脚本 `curl raw.githubusercontent.com` 卡住或慢

国内访问 GitHub raw 文件经常超时。换 jsdelivr CDN：

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/GeekASMR/network-ultra-server@main/scripts/install-from-source.sh | sudo bash
```

或者直接下载 tar.gz（GitHub release CDN 国内通常稳定）：

```bash
cd /tmp
curl -fL https://github.com/GeekASMR/network-ultra-server/archive/refs/heads/main.tar.gz -o nus.tar.gz
sudo bash -c 'rm -rf /opt/network-ultra-src && mkdir -p /opt/network-ultra-src && tar xzf /tmp/nus.tar.gz -C /opt/network-ultra-src --strip-components=1 && bash /opt/network-ultra-src/scripts/install-from-source.sh'
```

### Q3：装完了，`ss -tlnp | grep 18900` 显示在监听，但外网连不上

**原因 A — 云控制台安全组没放通**：腾讯云/阿里云/华为云在 web 控制台都有「云防火墙」或「安全组」配置，需要手动加入站规则放通 **TCP 18900 + UDP 18902**（两个都要，UDP 是 v1.2+ 主音频通道）。

**原因 B — 主机内核 iptables 拦了**（腾讯云轻量 / 阿里云 ECS 常见）：云盾 / YunJing 会注入 INPUT 规则。一行 flush：

```bash
sudo iptables -F INPUT
```

注意 flush 是临时的，重启会恢复。要永久解决就在控制台禁用云盾，或者用 `iptables-save > /etc/iptables/rules.v4` 持久化。

### Q4：服务起不来 / `journalctl` 报错

```bash
journalctl -u network-ultra-server -n 50 --no-pager
```

最常见的：端口被其他进程占了。`ss -tlnp | grep 18900` 看是谁在用。

### Q5：怎么彻底卸载

```bash
sudo systemctl disable --now network-ultra-server
sudo rm -f /etc/systemd/system/network-ultra-server.service
sudo rm -f /usr/local/bin/network-ultra-server
sudo rm -rf /etc/network-ultra /opt/network-ultra-src
sudo systemctl daemon-reload
```

---

## 配置文件

`/etc/network-ultra/config.toml`：

```toml
[server]
listen = "0.0.0.0:18900"              # WebSocket 监听
health_listen = "127.0.0.1:18901"     # 健康检查 + 指标（仅本地）
max_rooms = 50
max_peers_per_room = 8
max_connections = 200
admin_token = "<安装脚本自动生成>"

[tls]
enabled = false                       # 默认明文 ws，自建场景最常用
cert_file = ""
key_file = ""
auto_letsencrypt = false              # 切到 true + domain + email 走 Let's Encrypt
domain = ""
email = ""

[log]
level = "info"
format = "json"
path = ""                             # 空 = stdout（systemd 自动收集）

[ratelimit]
hello_per_ip_per_minute = 10
room_create_per_peer_per_minute = 5
room_join_per_peer_per_minute = 30
audio_frames_per_peer_per_second = 200
```

修改配置后：`sudo systemctl restart network-ultra-server`

## TLS 三种模式

| 配置 | 模式 | 适用场景 |
| --- | --- | --- |
| `enabled = false` | 明文 `ws://` | 朋友间 / 局域网 / 个人自建 |
| `enabled = true` + `cert_file` + `key_file` | 静态证书 | 已有 SSL |
| `enabled = true` + `auto_letsencrypt = true` + `domain` + `email` | Let's Encrypt 自动签发 | 公网域名 + wss:// |

> Auto-LE 需要服务器同时监听 80 / 443（HTTP-01 challenge）。

## 端点

- `ws://0.0.0.0:18900` — 客户端控制 + 音频兜底（TCP）
- `udp://0.0.0.0:18902` — 音频主通道（v1.2+；客户端通过 welcome 自动协商）
- `http://127.0.0.1:18901/healthz` — JSON 状态
- `http://127.0.0.1:18901/metrics` — Prometheus 指标

## 常用运维

```bash
systemctl status network-ultra-server
journalctl -u network-ultra-server -f
systemctl restart network-ultra-server
curl http://127.0.0.1:18901/healthz
curl http://127.0.0.1:18901/metrics
```

## 协议

- **控制消息**：WebSocket text frame，JSON UTF-8
  - `hello` / `welcome` / `room_create` / `room_join` / `room_left` / `peer_*` / `ping` / `pong` / `error`
  - `welcome` v1.2+ 携带 `udpEndpoint` + `udpToken`，客户端据此握手 UDP 数据面
- **音频消息**：WebSocket binary frame **或** UDP datagram（v1.2+ 默认走 UDP）
  - 24 字节定长 header（type + codec_id + sourcePeerId 16B + seq 2B + length 2B）+ payload
  - codec_id：0=PCM / 1=FLAC / 2=Opus 192k / 3=Opus 128k(默认) / 4=Opus 64k
  - PCM 一帧 1920 字节 + 24 头 ~ 3 Mbps，Opus 128k 一帧 ~256 字节，FLAC 一帧 ~700 字节
- **UDP 数据面（v1.2+）**：客户端先 WS 握手取 token，然后用 token 在 UDP 18902 上 hello。
  服务器绑定 source addr 后所有音频走 UDP，避免 TCP HOL 阻塞。
  握手失败时自动回落到 WebSocket binary frame（兼容老服务器和受限网络）。

服务器对 payload 完全不感知，只检查长度并 fan-out 给同房间其他 peer。

## License

服务器（本仓库）：MIT — 见 [LICENSE](./LICENSE)。

客户端 VST3 插件：闭源商业产品，不接受 PR / 源码请求。仅在 [Releases](https://github.com/GeekASMR/network-ultra-server/releases/latest) 提供 Windows 安装包。
