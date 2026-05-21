# Network Ultra Server

为 [Network Ultra](https://github.com/GeekASMR/WDM2VST-Ultra) Windows VST3 插件配套的中心转发服务器,让两个异地音乐人能在各自 DAW 内通过网络以**无损 FLAC**质量协作。

这是开源的服务端。客户端插件(`Network Ultra Send.vst3` / `Network Ultra Receive.vst3`)随 WDM2VST Ultra 一起发布,闭源。

## 一键安装(Linux,推荐)

SSH 到服务器,执行:

```bash
curl -fsSL https://raw.githubusercontent.com/GeekASMR/network-ultra-server/main/scripts/install-from-source.sh | sudo bash
```

脚本会自动完成:

1. 检测发行版与 CPU 架构(支持 x86_64 / aarch64)
2. 如未安装 Go 1.22,自动下载到 `/usr/local/go`(约 120 MB,只装一次)
3. 克隆本仓库到 `/opt/network-ultra-src`
4. 用 `go build` 编译出二进制,放到 `/usr/local/bin/network-ultra-server`
5. 生成 `/etc/network-ultra/config.toml`,自动产生随机 admin token
6. 注册 systemd 服务并启动
7. 调用 `/healthz` 验证服务存活

国内服务器一般 60 秒到 2 分钟即可完成。装完后,VST 插件填 `ws://<你的服务器 IP>:18900` 就能连。

**升级**:再跑一次同样的命令即可,自动拉新代码、重编译、重启服务。

## 替代方案:下载预编译二进制

(适用于不想在服务器上装 Go 的环境。)等 GitHub Actions 把二进制构建发布到 [releases](https://github.com/GeekASMR/network-ultra-server/releases/latest) 后,可以用:

```bash
curl -fsSL https://github.com/GeekASMR/network-ultra-server/releases/latest/download/install.sh | sudo bash
```

## 手动安装

1. 从 [releases](https://github.com/GeekASMR/network-ultra-server/releases/latest) 下载对应平台二进制:
   - `network-ultra-server-linux-amd64`
   - `network-ultra-server-linux-arm64`
   - `network-ultra-server-windows-amd64.exe`(实验性,不保证稳定)
2. 复制到 `/usr/local/bin/network-ultra-server` 并 `chmod +x`
3. 创建 `/etc/network-ultra/config.toml`(参考下面的示例)
4. 运行 `network-ultra-server -config /etc/network-ultra/config.toml`

## 配置文件

```toml
[server]
listen = "0.0.0.0:18900"              # WebSocket 公网监听
health_listen = "127.0.0.1:18901"     # 健康检查/指标(默认仅本地)
max_rooms = 50                        # 单服务器最大房间数
max_peers_per_room = 8                # 单房间最大成员数
max_connections = 200                 # 总连接数上限
admin_token = "<install.sh 自动生成的 32 字节 hex>"

[tls]
enabled = false                       # 默认明文 ws,自建场景常用
cert_file = ""                        # 静态证书路径
key_file = ""
auto_letsencrypt = false              # true 走 Let's Encrypt 自动签发
domain = ""                           # autocert 必填
email = ""

[log]
level = "info"                        # debug | info | warn | error
format = "json"                       # json | text
path = ""                             # 留空 = 输出到 stdout(systemd 会捕获)

[ratelimit]
hello_per_ip_per_minute = 10          # 同 IP 每分钟最多 10 次握手
room_create_per_peer_per_minute = 5
room_join_per_peer_per_minute = 30
room_list_per_peer_per_minute = 60
audio_frames_per_peer_per_second = 200
```

## 端点

- **WebSocket** `0.0.0.0:18900` — VST 客户端的控制 + 音频流双工通道
- **健康检查** `127.0.0.1:18901/healthz` — JSON 状态(运行时长 / 房间数 / 连接数)
- **指标** `127.0.0.1:18901/metrics` — Prometheus 文本格式

## TLS 三种模式

通过 `[tls]` 字段切换:

| 配置 | 模式 | 适用场景 |
|---|---|---|
| `enabled = false` | 明文 `ws://` | 朋友间 / 局域网 / 个人自建,无需证书 |
| `enabled = true` + `cert_file` + `key_file` | 静态证书 | 你已有 SSL 证书 |
| `enabled = true` + `auto_letsencrypt = true` + `domain` + `email` | Let's Encrypt 自动签发 | 公网域名,要支持公网 wss |

启用 autocert 后,服务器需要同时监听 80 + 443(HTTP-01 challenge 需要)。

## 常用运维命令

```bash
systemctl status network-ultra-server      # 看服务状态
journalctl -u network-ultra-server -f      # 实时日志(JSON 格式)
systemctl restart network-ultra-server     # 重启(客户端会自动重连)
curl http://127.0.0.1:18901/healthz        # 健康检查
curl http://127.0.0.1:18901/metrics        # 看指标
```

## 协议简介

- **控制消息**:WebSocket text frame,JSON UTF-8;包含 hello / room_create / room_join / room_list / ping 等
- **音频消息**:WebSocket binary frame,24 字节定长 header(type / sourcePeerId / seq / length)+ 后续 FLAC 帧载荷
- 完整协议参见 [requirements.md](https://github.com/GeekASMR/WDM2VSTUltra/blob/main/.kiro/specs/network-ultra/requirements.md)(私有仓,客户端工程师查阅)

## License

MIT — 见 [LICENSE](./LICENSE)。

Network Ultra 的**客户端插件**是闭源商业产品,只有这个服务器是 MIT 开源。
