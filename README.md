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

## 自建服务器（Linux 推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
```

脚本会：

1. 自动检测发行版 + CPU 架构（x86_64 / aarch64）
2. 没装 Go 1.22 就装一个到 `/usr/local/go`（约 120 MB，仅一次）
3. 克隆本仓库到 `/opt/network-ultra-src`，编译二进制
4. 生成 `/etc/network-ultra/config.toml` + 随机 32 字节 admin token
5. 注册 systemd 服务并启动
6. 调用 `/healthz` 验证存活

国内服务器约 1~2 分钟完成。装好后 VST 插件填 `ws://<你的 IP>:18900` 即可连接。

**升级**：再跑一次同样命令，自动拉新代码、重编译、重启。

> 腾讯云轻量、阿里云轻量等带"云防火墙"的实例：登录后台手动放通入站 18900 端口，并 `sudo iptables -F` 清空主机 iptables（这些云的安全组规则会重新注入，先 flush 一次主机 iptables 再加规则）。

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

## TLS 三种模式

| 配置 | 模式 | 适用场景 |
| --- | --- | --- |
| `enabled = false` | 明文 `ws://` | 朋友间 / 局域网 / 个人自建 |
| `enabled = true` + `cert_file` + `key_file` | 静态证书 | 已有 SSL |
| `enabled = true` + `auto_letsencrypt = true` + `domain` + `email` | Let's Encrypt 自动签发 | 公网域名 + wss:// |

> Auto-LE 需要服务器同时监听 80 / 443（HTTP-01 challenge）。

## 端点

- `ws://0.0.0.0:18900` — 客户端控制 + 音频
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
- **音频消息**：WebSocket binary frame
  - 24 字节定长 header（type + sourcePeerId 16B + seq 2B + length 2B）+ payload
  - 当前 payload = PCM 16-bit @ 48kHz / Stereo / 480 sample/帧（10ms/帧）
  - 一帧约 1920 字节 + 24 头 = 1944 字节，~3 Mbps 单向

服务器对 payload 完全不感知，只检查长度并 fan-out 给同房间其他 peer。

## License

服务器（本仓库）：MIT — 见 [LICENSE](./LICENSE)。

客户端 VST3 插件：闭源商业产品，不接受 PR / 源码请求。仅在 [Releases](https://github.com/GeekASMR/network-ultra-server/releases/latest) 提供 Windows 安装包。
