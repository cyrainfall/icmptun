# icmptun — ICMP + FEC 隧道（中文）

在「TCP/UDP 被定向干扰、只有 ICMP 能过」的链路上，把 TCP 流量塞进 ICMP echo 报文里的自研隧道。

- Go 编写，**只用标准库**，约 1.6k 行，编译产物是单文件静态二进制；
- 客户端在本地开 **SOCKS5**（可选再开一个 HTTP CONNECT）代理，服务端收到 OPEN 帧才去 `net.Dial`；
- 60 字节帧头 + **256 位 SACK 位图** + **XOR FEC** + 按真实进展判活的看门狗；
- 实测：170ms RTT 的路径上 **1.24–1.39 MB/s** 下载。

```
 应用 ──SOCKS5 127.0.0.1:1080──┐                        ┌── 目标站点
 应用 ──HTTP   127.0.0.1:8118──┤ mux ── ICMP echo ────── ┤
                               └── 客户端 ──────────────── 服务端 ── net.Dial
```

> 英文文档（更完整，含与同类项目的对比）：[README.md](README.md)

---

## 它是什么 / 不是什么

| 是 | 不是 |
|---|---|
| 在只剩 ICMP 的链路上跑 TCP 的代理 | VPN：没有 tun/tap、不做 IP 转发，只代理 TCP |
| 一个可测量的协议实现（有实测表、有失败记录） | 隐蔽通道：它就是一条裸 ICMP 隧道，不伪装 |
| 一个下午能读完的单文件程序 | 生产级基础设施：没有高可用、当前树里没有自动 failover |

## 安装

**预编译二进制**（Linux / macOS / Windows，amd64 与 arm64）在
[Releases](https://github.com/cyrainfall/icmptun/releases)，同页附 `SHA256SUMS`。
单文件静态二进制，无运行时依赖，不需要配置。

**从源码构建**（Go 1.22+）：

```sh
go build -o icmptun .
```

## 不联网也能验证

```sh
$ ./icmptun -mode selftest
icmptun selftest: 8 checks, no raw socket needed
  ok    1/8  frame round-trip preserves every field
  ...
8 passed, 0 failed in 412µs
```

不需要 root、不开 socket、不发出任何包。覆盖帧布局、FEC 校验包生成与丢包恢复、
SACK 位图极性、mux 帧格式、ICMP 校验和与 `-suppress` 行为。
`go test ./...` 跑的是同一套检查。

## 30 秒起步

两端都需要 raw socket 权限（root / 管理员）。

```sh
go build -o icmptun .        # 无第三方依赖

# 服务端（能正常访问互联网的那台机器）
sudo ./icmptun -mode server -payload 1350 -wnd 4096 -fec 8 -pps 20000 -v

# 客户端（受限机器，203.0.113.10 换成你的服务端）
sudo ./icmptun -mode client -peer 203.0.113.10 \
  -socks 127.0.0.1:1080 -http 127.0.0.1:8118 \
  -payload 1350 -wnd 4096 -fec 8 -pps 20000 -suppress=false -v

# 验证
curl -x socks5h://127.0.0.1:1080 -o /dev/null \
  -w '%{speed_download} B/s\n' https://cachefly.cachefly.net/10mb.test
```

两端 `-payload` 与 `-fec` **必须一致**。如果 `rxfrm 0/s`（帧根本发不出去），先试 `-suppress=false`。

## 协议要点

### 帧头 60 字节（大端定长）

| 偏移 | 长度 | 字段 | 说明 |
|---:|---:|---|---|
| 0 | 4 | magic `DTH1` | `0x44544831` |
| 4 | 1 | version | 当前 `1` |
| 5 | 1 | flags | `1` ack-only、`2` parity、`4` ping、`16` from-client |
| 6 | 4 | session | 服务端见到新 session 就采纳 |
| 10 | 4 | seq | |
| 14 | 4 | cum-ack | 以下全部收到 |
| 18 | 2 | window | 空闲接收槽位（包） |
| 20 | 32 | **SACK 缺口位图** | 256 位；**置位 = 该 seq 丢失** |
| 52 | 4 | FEC block | |
| 56 | 1 | parity index | |
| 57 | 2 | payload 长度 | |
| 59 | 1 | **link id** | 同一台服务器可跑多条车道 |

默认 payload 1350 B：`60 + 1350 = 1410`，加 IP/ICMP 头后 1438 B，低于常见 1450 MTU。

### 把吞吐从 0 抬到 1.3 MB/s 的五件事

1. **镜像客户端的 ICMP id/seq**：CGNAT 只放行与出向请求匹配的 echo reply，所以服务端不自造
   id/seq，而是复制客户端最近一次的值。
2. **服务端采纳新 session**：否则两端都在等对方让位，握手永远完成不了。
3. **SACK 位图语义**：置位是「丢」，不是「收到」。搞反就是重传风暴。
4. **抬高接收缓冲**：macOS raw socket 默认缓冲远小于 4096 包的窗口，不 `setsockopt` 会在用满
   窗口之前就溢出。
5. **64 → 256 位 SACK（决定性）**：这是算术题，不是调参——

   ```
   RTT 170ms，目标 1.3 MB/s
     在途数据 = 1.3e6 × 0.17 ≈ 221 KB ≈ 164 个 1350B 包
     64 位位图  → 64 × 1350 / 0.17 ≈ 508 KB/s   ← 正是观测到的 ~500KB/s 上限
     256 位位图 → ≈ 2.0 MB/s，扣掉 K=8 的 ~11% FEC 开销 ≈ 1.8；实测 1.3
   ```

   cum-ack 被单个丢包卡住时，**位图宽度就是高 RTT 路径上的吞吐**。

### 两个沉默杀手

- **`-suppress`（默认开）**：故意写坏 ICMP 校验和，压掉对端内核的自动 echo 回包。但有些上网
  路径会直接丢弃校验和错误的 ICMP，表现为隧道一帧都发不出去：客户端 `rxfrm 0/s`、服务端
  tcpdump 看不到任何 echo request。这类路径必须 `-suppress=false`。
- **看门狗量的是「真实进展」**：`sndUna` 推进或 `rcvNext` 推进，才算活着。如果按「有没有收到帧」
  判活，对端把状态 reset 之后，它的帧照样流动、`rxfrm` 看着很健康、`inflight` 却永远清不掉、
  `acked` 一动不动 —— 隧道就此静默死亡，只能人工重启。

看门狗触发后 `os.Exit(3)`，由外部守护（systemd / launchd `KeepAlive` / Windows 的 `while`
循环）拉起，会话从头开始。**不要**在进程内「换 session 继续跑」：老会话残留的半帧会让 mux
读到垃圾长度（`invalid frame ... session desync`），解析器永久错位。

## 实测

参数 `-wnd 4096 -fec 8 -payload 1350 -pps 20000`，RTT ≈ 170ms。

| 路径 | 方向 | 吞吐 |
|---|---|---|
| macOS 客户端 | 下载 | **1.24 – 1.39 MB/s** |
| macOS 客户端 | 上传 | 2.09 MB/s |
| Windows 客户端（`-suppress=false`） | 下载（首次） | 634 KB/s |
| Windows 客户端 | 下载（第二次） | 1.11 MB/s |
| Windows 客户端 | 上传 | 3.25 MB/s |
| 同机对 TLS 中继（对照） | 下载 | 360 KB/s |
| 裸 ICMP，不走隧道（路径上限） | — | 2.2 – 3.7 MB/s |

相当于裸 ICMP 下限（2.2 MB/s）的 ~60%、上限（3.7 MB/s）的三分之一左右；差距来自 FEC 开销与丢包，不是帧头。

## 部署

### systemd（服务端）

```ini
# /etc/systemd/system/icmptun.service
[Unit]
Description=icmptun server
After=network-online.target

[Service]
ExecStart=/opt/icmptun/icmptun-linux -mode server -payload 1350 -wnd 4096 -fec 8 -pps 20000 -v
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

多条车道就是在同一台机器再起一个实例，`-linkid N` 与客户端成对：

```sh
/opt/icmptun/icmptun-linux -mode server -linkid 1 -payload 1350 -wnd 4096 -fec 8 -pps 20000 -v
```

### macOS（launchd，客户端）

模板在 `scripts/com.cyx.icmptun.plist`，用安装脚本渲染并装入（peer / linkid 由参数给，不写死）：

```sh
sudo scripts/install-mac-lane.sh 203.0.113.10 0
```

之后：

```sh
sudo launchctl kickstart -k system/com.cyx.icmptun     # 重启
sudo launchctl print    system/com.cyx.icmptun | head  # 状态
tail -f /var/log/icmptun-client.log                    # 日志
```

`scripts/icmptun-ctl.sh` 是一个更轻的本地启动器（`ICMPTUN_PEER=... scripts/icmptun-ctl.sh start`）。

### Windows（计划任务，客户端）

1. 二进制放到 `C:\icmptun\`，用 `run-main.ps1` 的 `while` 循环守住（退出 3 秒后重启）；
2. 挂开机计划任务，**以 SYSTEM / 最高权限运行**，否则 `net.ListenIP` 建不出 raw socket；
3. 如果你同时跑全隧道 VPN，需要给服务端加一条走物理网卡的主机路由，否则隧道的 ICMP 会被
   半路由抢走，隧道自己把自己套住：

```powershell
New-NetRoute -DestinationPrefix 203.0.113.10/32 -InterfaceIndex <物理网卡索引> `
  -NextHop 192.168.1.1 -RouteMetric 1 -PolicyStore PersistentStore
```

`-InterfaceIndex` 要用数字（中文别名在 `-InterfaceAlias` 下匹配不上）。

### 接进 Clash / mihomo

它就是一个 socks5 出口：

```yaml
proxies:
  - {name: ICMP-隧道, type: socks5, server: 127.0.0.1, port: 1080, udp: false}
proxy-groups:
  - {name: 出口, type: select, proxies: [ICMP-隧道]}
```

## 参数

| 参数 | 默认 | 含义 |
|---|---|---|
| `-mode` | `client` | `client` / `server` |
| `-peer` | — | 服务端 IPv4（仅客户端） |
| `-socks` | `127.0.0.1:1080` | 本地 SOCKS5 监听 |
| `-http` | — | 可选 HTTP CONNECT 代理 |
| `-L` | — | 额外端口转发 `lport:host:rport,...` |
| `-linkid` | `0` | 车道号 0–255，不匹配的帧直接丢 |
| `-payload` | `1300` | 每帧净荷（1450 MTU 路径用 1350） |
| `-fec` | `8` | 每 K 个数据包补 1 个 XOR 校验包，0 关闭 |
| `-wnd` | `512` | 接收窗口（包） |
| `-pps` | `4000` | 发送令牌桶上限 |
| `-watchdog` | `15` | 客户端：有活待发且停滞 N 秒后重置（0 关闭） |
| `-suppress` | `true` | 用坏校验和压掉对端内核 echo |
| `-v` | `false` | 详细日志 |

## 排障

| 现象 | 最可能的原因 |
|---|---|
| 客户端 `rxfrm 0/s`，服务端 tcpdump 没有 echo request | 路径丢弃坏校验和 ICMP → `-suppress=false` |
| `rxpkt 0/s` + `rxfrm` 健康 + `acked` 冻住 | 会话失步；等看门狗，或两端都重启 |
| `ooo` 堆到几百 | 对端在等一个永远不来的序号（同上） |
| 跑满一段时间后 0 B/s | 链路限流，不是协议 bug —— `ping -c 3 -s 1200 <server>` 看大包丢不丢 |
| 启动后第一条请求卡约 10 秒 | 服务端还在采纳新 session，先随便 `curl` 一次热身 |
| 170ms RTT 下卡在 ~500 KB/s | 位图/窗口对这条路径太小 |

## 已知限制

1. **车道会被间歇限流**：持续高速跑十几到几十 MB 后大包 ICMP 成片丢，隧道 0 B/s，几分钟后
   自行恢复。判断方法：`ping -c 3 -s 1200 <server>`，大包丢就是车道降级。
2. **`-suppress` 在部分路径上完全不通**（见上）。
3. **`-fec 2/4` 有未知问题（0 B/s）**，只用 `8` 或 `0`。这是本仓库最大的未解问题。
4. **`-dup` 无效**：链路按 id/seq 去重后才到我们这层。
5. **限流与 pps 无关**：实测 4000 与 20000 表现一致。
6. **冷启动第一条请求会卡 5–13 秒**。
7. **客户端被 `kill -9` 后服务端可能停在旧会话**，重启服务端清一次即可。
8. **当前只有单车道，没有自动 failover**：路挂了就是挂了，等它恢复或看门狗重启。

## 未解问题

- `-fec 2/4` 为什么 0 B/s，而 `8` 和 `0` 正常？
- 持续负载后的吞吐塌陷，是 pps 整形、队列占满，还是上游按 payload 大小限流？
- RTT > 300ms 时，256 位位图的天花板在哪？

## 与同类项目

| 项目 | 做法 | 差异 |
|---|---|---|
| [icmptunnel](https://github.com/DhavalKapil/icmptunnel) | tun/tap，把 IP 包塞进 ICMP（2016，C） | 它透传整个 IP 接口；本项目是多路 TCP + FEC + SACK，另有 Windows raw socket 实现 |
| [ICMPTunnel](https://github.com/Qteam-official/ICMPTunnel) | 反向 TCP 转发 | 单端口转发；本项目是通用 SOCKS5/HTTP 出口 |
| [udp2raw](https://github.com/wangyu-/udp2raw)、[phantun](https://github.com/dndx/phantun) | 把 UDP/TCP 伪装成别的协议 | 同一工程母题，不同层 |

## 法律与伦理

这是一个网络实验与协议实现。ICMP 隧道可能违反你的 ISP 服务条款、主机商 AUP 或当地法律。
请只在你拥有或获授权的网络上使用。作者不对误用负责。

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。Copyright 2026 cyrainfall。
