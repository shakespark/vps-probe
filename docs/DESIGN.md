# VPS 探针设计文档

## 0. 背景与目标

之前使用哪吒探针，面板被攻破后所有 VPS 均被控制，只能全部重装。根本原因在于架构：面板持有对全部 agent 的 root 命令通道。本项目的首要目标是**即使服务端被完全攻破，也无法控制任何一台 VPS**。

功能需求：

| # | 需求 | 说明 |
|---|---|---|
| F1 | VPS 之间时延 | N×N 互测，ICMP，记录 min/avg/max/抖动/丢包，可图形化查看历史 |
| F2 | 资源监控 | CPU、内存、Swap、磁盘占用、负载、网络速率，图形化查看历史 |
| F3 | 告警 | 资源阈值、离线、时延/丢包、流量配额，通过 Telegram 推送 |
| F4 | 月流量统计 | 物理网口收/发总量，按周期（默认自然月，可配置账单日）累计，**重启不丢数据** |

规模：≤10 台 Linux VPS（KVM 等完整虚拟化，无容器型）。

## 1. 安全原则（硬性约束）

1. **agent 只出不进**：不监听任何端口，只主动向服务端发送 UDP 上报包；唯一接收的是服务端对上报包的加密确认（ACK），ACK 里只有已收到的报文 ID。
2. **agent 不接受任何指令**：不执行命令、不下载脚本、不远程更新、不从服务端拉取配置。agent 只从服务端接收 ACK，ACK 仅用于把已送达的报文移出重传队列，不含任何其他语义。
3. **agent 非 root 运行**：专用系统用户 `vps-probe`，systemd 加固；采集数据只读 `/proc`、`/sys`、`statfs`，都不需要 root。
4. **互测目标由 agent 本地配置**：不由服务端下发，防止服务端被利用去探测任意地址。
5. **Web 不暴露公网**：Web 只监听 `127.0.0.1`，只能经 Cloudflare Tunnel + Access 登录后访问。服务端唯一的公网端口是 UDP ingest（agent 上报），**校验不通过的包一律静默丢弃、不做任何回应**，端口扫描器无法把它与被防火墙过滤的端口区分开（见 §6.2）。
6. **Web 只读**：Web 没有任何写接口，所有配置通过服务端配置文件修改。
7. **Telegram Bot 只发不收**：不设置 webhook、不调用 getUpdates，不存在通过 TG 下达指令的通道。
8. **每台 agent 独立 token**：token 派生出该节点的加密密钥（§5.3），一台的 token 泄露只影响该节点的数据。服务端需要用 token 解密，因此服务端配置文件中保存 token 明文，文件权限 0600。

最坏情况分析：服务端被攻破 → 攻击者能看到/篡改监控数据、能用 TG bot 发消息，但**无法在任何 agent 上执行代码**。

## 2. 总体架构

```
 ┌──────────── VPS × N ────────────┐
 │ vps-probe-agent (非 root)        │
 │  ├ 采集 /proc /sys statfs         │
 │  ├ ICMP 互测 peers（本地配置）     │
 │  ├ 流量累计 → 本地状态文件         │
 │  └ 每 10s 发送加密 UDP 包 ────────┼──┐  公网直连
 └─────────────────────────────────┘  │  UDP + protobuf + XChaCha20-Poly1305
                                      │  ▲ 仅对合法包回 ACK
 ┌──────────── 服务端 VPS ───────────┐ │
 │ vps-probe-server                  │ │
 │   ├ :9527/udp         ingest ◄────┼─┘  非法包静默丢弃
 │   ├ 127.0.0.1:8080 Web + 只读 API │
 │   ├ SQLite (WAL)                  │
 │   ├ 降采样 / 过期清理              │
 │   └ 告警引擎 → api.telegram.org    │
 │ cloudflared ──→ 127.0.0.1:8080    │ ◄── 浏览器 → Cloudflare Access 登录
 │ vps-probe-agent（本机也装一份）     │
 └───────────────────────────────────┘
```

- **ingest 与 Web 完全独立**：ingest 是 UDP，只接受加密上报包，不提供任何查询；Web 只绑定 `127.0.0.1`，公网无法直接访问。
- Web：cloudflared 把 `probe.example.com` 映射到 `127.0.0.1:8080`，Cloudflare Access 配置登录策略（邮箱 OTP / GitHub 等）。服务端可选校验 `Cf-Access-Jwt-Assertion` JWT（纵深防御，推荐开启）。
- 服务端宕机或网络中断时：监控曲线会出现缺口（agent 内存缓冲 1 小时，恢复后补传），**月流量不受影响**（在 agent 本地累计，见 §4）。

## 3. 技术选型

| 部分 | 选型 | 理由 |
|---|---|---|
| 语言 | Go 1.24 | 单文件静态二进制，交叉编译 amd64/arm64 方便 |
| 数据库 | SQLite（`modernc.org/sqlite`，纯 Go） | 10 台规模绰绰有余，无需 cgo，备份就是一个文件 |
| 前端 | 原生 JS + ECharts，`go:embed` 打包进二进制 | 无构建链；ECharts 本地打包，不引用外部 CDN |
| 上报编码 | protobuf（`google.golang.org/protobuf`） | 紧凑，一份上报约 600 字节，单个 UDP 包即可装下；生成代码提交进仓库，日常开发无需 protoc |
| 加密 | XChaCha20-Poly1305（`golang.org/x/crypto`）+ HKDF-SHA256（标准库） | 加密与防伪造一步完成；24 字节随机 nonce 无需计数器 |
| ICMP | `golang.org/x/net/icmp` | 支持非特权 datagram ICMP socket |
| 配置 | YAML（`gopkg.in/yaml.v3`） | 可读性好，支持注释 |
| 部署 | systemd + 安装脚本 | 不依赖 Docker |

依赖尽量少，每个第三方库都要能说出存在的理由。

## 4. 月流量统计（重点）

### 4.1 网卡选择

- 默认自动识别：`/sys/class/net/<if>/device` 存在的网卡视为物理网卡（KVM 下 virtio 网卡也有 device 链接），排除 `lo`、`docker*`、`veth*`、`br-*`、`wg*`、`tun*`、`tailscale*` 等。
- 可在配置中显式指定 `interfaces: [eth0]`，指定后以配置为准；不写或写空列表即自动识别。
- 多块物理网卡时分别统计，并提供合计值。

### 4.2 累计算法

数据源：`/proc/net/dev` 中的 rx_bytes / tx_bytes（内核 64 位计数器，开机后从 0 开始）。

本地状态文件 `/var/lib/vps-probe/traffic.json`：

```json
{
  "version": 1,
  "boot_id": "c1a7...",
  "interfaces": {
    "eth0": {
      "last_rx": 123456789, "last_tx": 98765432,
      "periods": {
        "2026-09-01": { "rx": 51234567890, "tx": 40123456789 },
        "2026-08-01": { "rx": 61234567890, "tx": 50123456789 }
      }
    }
  }
}
```

每次采样（10s）：

1. 读取当前 `boot_id`（`/proc/sys/kernel/random/boot_id`）和计数器 `cur`。
2. 计算增量：
   - `boot_id` 与状态文件不同（机器重启过）→ `delta = cur`（开机以来的全部流量）
   - `boot_id` 相同且 `cur >= last` → `delta = cur - last`
   - `boot_id` 相同但 `cur < last`（驱动重载/网卡重建导致计数器归零）→ `delta = cur`
3. 把 `delta` 累加到**当前时间所属周期**，更新 `last = cur`、`boot_id`。
4. 首次见到某块网卡（首次安装，或新加入统计的网卡）时没有历史读数：若本次开机时间晚于当前周期起点，开机以来的流量全部属于本周期，`delta = cur`；否则无法判断其中有多少属于本周期，只记录基线、不计入。
5. 落盘：每 30s 一次 + 收到 SIGTERM 时一次。写法为写临时文件 → fsync → rename → fsync 目录，保证不会写出半个文件。
6. 周期记录保留最近 24 个。
7. 状态文件损坏（无法解析）时，将其改名为 `traffic.json.corrupt-<时间>` 保留现场，按首次运行处理并记录错误日志。

数据丢失窗口分析：

| 场景 | 丢失 |
|---|---|
| 正常重启 / 关机（systemd 先停 agent） | 0（SIGTERM 时落盘）；agent 停止到内核关闭之间的极少量流量除外 |
| agent 崩溃 / 升级重启 | 0（boot_id 不变，下次启动用 `cur - last` 补回） |
| 服务端宕机 / 网络中断 | 0（在 agent 本地累计，恢复后上报总量） |
| 宿主机断电 / 内核 panic | ≤ 最后 30s 的流量 |
| agent 被停用期间机器又重启过 | 停用到重启之间的流量（无法避免，属于运维操作） |

### 4.3 周期计算

- 配置 `timezone`（默认 `Asia/Shanghai`）、`reset_day`（默认 `1`，即自然月）和可选的 `reset_time`（`"HH:MM"`，默认 `00:00`）。
- 周期以起始日期命名（如 `2026-09-01`）。`reset_day=15` 表示每月 15 日 00:00 起算；`reset_day: 21` + `reset_time: "18:21"` 表示每月 21 日 18:21 起算（按购买时刻重置的商家）；`reset_day` 大于当月天数时取当月最后一天（如 31 在 2 月取 28/29 日），时刻不变。
- 按日流量以自然日记录：`reset_time` 不是 00:00 时，重置当天的记录归新周期，旧周期的按日明细里缺最后那半天（周期总量不受影响）。
- 修改已有节点的 `reset_day` / `reset_time` 后，周期起始日期变了，服务端里旧起始日期的那条记录不会自动消失；若它比新周期的起始日期更晚，页面会把它当成当前周期，需要手动删掉（见 README「排查」）。
- 增量归入**采样时刻**所属的周期。10s 采样粒度下跨月边界的误差可忽略。
- 若 agent 在月末停止、次月才启动，停止期间积攒的增量会计入新周期（已知限制，写入文档）。

### 4.4 上报与服务端存储

- agent 每次上报**当前周期和上一周期的累计总量**（而不是增量）。服务端做幂等 upsert，丢包、重复都不会导致重复计数；月初上报失败时，上一周期的最终值也能补齐。
- 服务端对每个 `(node, iface, period_start)` 保存总量及其报文 `ts`，**只接受 `ts` 更新的报文**（latest-ts-wins）。重放的旧包 `ts` 旧（`ts` 在密文里，无法篡改），会被忽略；agent 状态文件重建后总量合法变小，但新报文 `ts` 更新，服务端照常跟随。
- **按日流量**：服务端记录每个 `(node, iface, 日期)` 当天最后一次看到的本周期总量，某日用量 = 当日值 − 前一日值（同一周期内；周期首日前一日视为 0；差值为负说明 agent 状态重建过，取当日值）。日期按服务端 `timezone` 划分，**必须与各 agent 的 `traffic.timezone` 一致**（默认都是 `Asia/Shanghai`）。
- agent 状态文件是权威数据源；服务端是副本加展示层。

## 5. Agent 设计

### 5.1 采集项

| 类别 | 指标 | 来源 | 频率 |
|---|---|---|---|
| CPU | 使用率（总体）、steal | `/proc/stat` 差分 | 10s |
| 负载 | load1/5/15 | `/proc/loadavg` | 10s |
| 内存 | total / available / used、swap total/used | `/proc/meminfo`（used = total − MemAvailable） | 10s |
| 磁盘 | 每个挂载点 used/total、inode 使用率 | `statfs`，挂载点可配置，默认 `/` | 60s |
| 网络 | 各物理网卡 rx/tx 速率 | `/proc/net/dev` 差分 | 10s |
| 流量 | 当前/上一周期累计 | 见 §4 | 10s |
| 系统 | uptime、内核版本、主机名、CPU 核数 | `/proc/uptime` 等 | 启动时 + 每小时 |
| 时延 | 每个 peer 的 sent/lost/min/avg/max/jitter | ICMP | 每秒 1 包，10s 汇总 |

CPU steal 单独记录，便于发现超售的机器。

### 5.2 ICMP 互测

- 优先使用**非特权 ICMP datagram socket**（`SOCK_DGRAM` + `IPPROTO_ICMP`），需要 `net.ipv4.ping_group_range` 包含 agent 的 gid。systemd ≥ 243 的发行版默认 `0 2147483647`，一般无需改动。
- 不满足时回退为 systemd `AmbientCapabilities=CAP_NET_RAW`（只授予这一项能力）。
- 安装脚本会检测并提示走哪种方式。
- 每个 peer 每秒发 1 个包，超时 2s；每 10s 汇总一次 sent/lost/min/avg/max/jitter（jitter 为相邻 RTT 差绝对值的均值）。
- peers 在 agent 本地配置，既可以是其他 VPS，也可以是外部目标（如 `1.1.1.1`）。
- IPv4/IPv6 均支持，按配置的地址族决定。
- 后续可选：TCP 连接时延。测量时连接对端**已有的端口**（如 SSH 22）即可，不需要新开监听端口。首版不做。

### 5.3 上报协议

**传输**：UDP，默认端口 9527。单包上限 1200 字节（低于常见 MTU，避免 IP 分片）；超出时把 ping 结果拆到额外的包里，各包独立确认。

**包格式**：

```
+---------+------+----------+-------------+------------+------------------------------+
| ver (1) | type | node_len | node (1-32) | nonce (24) | ciphertext + Poly1305 tag(16) |
+---------+------+----------+-------------+------------+------------------------------+
ver = 1；type：1 = Report（agent → server），2 = Ack（server → agent）
```

- 密钥：`key = HKDF-SHA256(secret=token, salt="vps-probe/v1", info=node)`，32 字节。
- 加密：XChaCha20-Poly1305，nonce 每包随机生成；`ver | type | node_len | node` 作为附加认证数据（AAD），因此篡改头部、把 ACK 反射成 Report 都会解密失败。
- 明文：Report 或 Ack 的 protobuf 编码（§5.4）。

**服务端处理顺序**（§6.2）：检查长度与版本 → 按 node 查密钥（未知节点丢弃）→ 解密（失败丢弃）→ 解析 protobuf → 回加密 ACK。任何一步失败都不回应。

**可靠性**：

- 每个 Report 带随机 64 位 `id`；服务端收到后回 `Ack{ids}`。
- agent 维护一个未确认队列（上限 360 条，约 1 小时，超出丢弃最旧的；超过 2 小时的也丢弃）。新报文立即发送；5s 内未确认的，之后重发。
- 服务端在线（30s 内收到过 ACK）时，每秒最多重发 20 条积压报文，优先发最新的；服务端离线时只重发最新的一条作为探测，收到 ACK 后再补传积压。
- agent 使用**已连接的 UDP socket**（`connect()` 到服务端 IP:端口），内核会丢弃来自其他任何地址的数据报，agent 不接收公网上任意来源的包。
- 服务端离线期间每 60s 重新 `connect()` 一次（服务端地址是域名时会重新解析）。
- 服务端对非法包静默丢弃，因此 token 写错、node 不一致、防火墙/安全组未放行、时钟偏差过大，在 agent 看来都和"服务端宕机"一样。agent 启动后或上次 ACK 之后超过 2 分钟没收到 ACK 时打印 WARN 日志并列出这些可能原因（10 分钟重复一次），方便首次部署排查。
- 队列不落盘：流量总量已单独落盘，监控曲线出现缺口可以接受。

**防重放**：不单独做序列号，依靠数据幂等性：

- 监控数据按 `(node, ts)` 存储，重复写入结果相同；
- 流量总量在同一周期内只增不减，重放旧包无法改小；
- 告警与在线状态只基于**报文中的最大 `ts`**，而不是服务端收包时间，否则重放抓到的旧包就能让一台已离线的机器在 2h 内看起来仍然在线；
- 服务端拒绝 `ts` 与当前时间偏差超过 2h 的报文。

没有密钥就无法构造新报文，重放旧报文也改变不了任何状态。

### 5.4 报文定义（protobuf）

见 `proto/probe/v1/probe.proto`，要点：

```proto
message Report {
  int64  ts = 1;                 // 采样时间，unix 秒，对齐到 interval 整数倍
  uint64 id = 2;                 // 随机，用于 ACK
  SysInfo sys = 3;               // 启动时 + 每小时
  CPU cpu = 4;
  Load load = 5;
  Mem mem = 6;
  repeated Disk disks = 7;       // 每 60s
  repeated NetRate net = 8;      // 各网卡速率，字节/秒
  repeated IfaceTraffic traffic = 9;  // 当前 + 上一周期累计
  repeated Ping pings = 10;
}
message Ack { repeated uint64 ids = 1; }
```

同一 `(node, ts)` 可能拆成多个包（例如 ping 结果单独一个包），服务端按字段合并。

### 5.5 agent 配置示例

```yaml
node: hk-1
server:
  addr: 203.0.113.1:9527    # UDP；也可以写域名
  token: "<node token>"     # 由 vps-probe-server gen-token 生成
interval: 10s
state_dir: /var/lib/vps-probe
disks: ["/"]
interfaces: []              # 空 = 自动识别物理网卡；或 [eth0]
traffic:
  timezone: Asia/Shanghai
  reset_day: 1              # 1 = 自然月
ping:
  interval: 1s
  timeout: 2s
  peers:                    # name 要写成对端在服务端登记的节点 id，时延矩阵才能对上
    - { name: jp-1, addr: 203.0.113.5 }
    - { name: us-1, addr: 198.51.100.7 }
```

### 5.6 systemd 加固

见 `deploy/vps-probe-agent.service`，要点：

- `User=vps-probe`，`StateDirectory=vps-probe`（mode 0700），`UMask=0077`。
- `CapabilityBoundingSet=` 为空；ICMP 走非特权 datagram socket。主机不允许时改为 `CAP_NET_RAW`。
- `ProtectSystem=strict`、`ProtectHome=read-only`（不能用 `yes`，否则对 /home 下挂载点做 statfs 时，得到的是遮盖用的空 tmpfs 的数据）、`PrivateTmp`、`PrivateDevices`、`ProtectProc=invisible`、`ProtectKernel*`、`ProtectClock`、`ProtectHostname`。
- `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`、`RestrictNamespaces`、`MemoryDenyWriteExecute`、`SystemCallFilter=@system-service`、`MemoryMax=64M`。
- **不能加 `PrivateNetwork`**：`/proc/net/dev` 按网络命名空间隔离，加了之后只能看到 `lo`。
- 不能加 `ProcSubset=pid`：它会隐藏 `/proc/stat`、`/proc/meminfo`。
- `TimeoutStopSec=15`：留出退出前最后一次读数和落盘的时间。

## 6. 服务端设计

### 6.1 节点注册

节点只能在服务端配置文件中登记，Web 上不能添加：

```yaml
listen:
  web: 127.0.0.1:8080        # 只给 cloudflared 用
  ingest: ":9527"           # UDP，agent 上报（IPv4 + IPv6）
db: /var/lib/vps-probe-server/probe.db   # 全部数据都在这一个文件里
timezone: Asia/Shanghai    # 按日流量的日期划分，须与 agent 的 traffic.timezone 一致
cf_access:                 # 可选：校验 Web 请求的 Access JWT（见下）
  team_domain: myteam.cloudflareaccess.com
  aud: "<application AUD tag>"
nodes:
  - id: hk-1
    name: 香港 1
    token: "<node token>"      # 配置文件需为 root:vps-probe-server 0640
    traffic_quota_gb: 1000     # 可选，用于显示进度和告警
    traffic_quota_mode: sum    # sum(收+发) | max(取大) | tx | rx，不同服务商计费方式不同
telegram:                  # 可选；不配置则只记录不发送
  bot_token: "<token>"
  chat_id: "<chat id>"
alerts: [...]              # 见 §7；不写则用默认规则
offline_after: 30s         # 超过多久没有新报文算离线，约 agent interval 的 3 倍
retention:
  raw: 48h                 # 10s 原始数据
  m5: 30d                  # 5 分钟聚合
  h1: 400d                 # 1 小时聚合
backup:
  dir: /var/lib/vps-probe-server/backups
  keep: 7                  # 每日一份
```

**cf_access**：配置后，每个网页请求（`/`、`/static/`、`/api/`）都必须带 Cloudflare Access 注入的 `Cf-Access-Jwt-Assertion`，否则 403：

- 只接受 RS256；校验签名、`aud`（可为数组，须包含配置的 AUD tag）、`iss` = `https://<team_domain>`、`exp`/`nbf`（30s 容差）。
- 公钥从 `https://<team_domain>/cdn-cgi/access/certs` 获取，每小时刷新；遇到未知 `kid`（密钥轮换）立即重新获取，但每分钟最多一次，防止伪造 kid 刷请求。
- 启动时不等待公钥：取到之前所有请求都 403，后台每 30s 重试（fail closed）。
- 拒绝原因按类别限频写日志。开启后在服务器上直接 `curl 127.0.0.1:8080` 也会 403，属预期。

节点的 `addr`（其他节点 ping 它的地址）、`reset_day` 和 `reset_time` 只供 `vps-probe-server agent-config` 生成 agent.yml 使用；agent 自己的配置文件仍是唯一依据，agent 不从服务端获取任何配置。

提供 `vps-probe-server gen-token` 子命令：生成 32 字节随机 token（base64url），同一个值同时填进 agent 和服务端配置。服务端启动时检查配置文件权限：其他用户可读或组可写则拒绝启动。推荐 `root:vps-probe-server 0640`，服务能读但不能改。

### 6.2 Ingest

- 单个 UDP socket 接收，包长 < 最小头部或 > 1200 字节、版本不对、node 未登记、解密失败、protobuf 解析失败、`ts` 偏差超过 2h、node 与包头不一致——**任何一种情况都静默丢弃，不回应**。
- 解密前只执行"读头部 + 查表 + AEAD 校验"，未认证的数据不会进入 protobuf 解析器，暴露给攻击者的代码面很小。
- 只对通过校验的包回 ACK，ACK 大小远小于请求，不存在放大攻击。
- 丢弃事件按原因计数（不逐条打日志，避免被刷日志），在 Web 上显示。
- 数值做范围检查（百分比 0-100、时延非负等），不合理的字段丢弃。
- 防火墙建议：只对各 VPS 的 IP 放行 9527/udp（可选，安装脚本给出示例规则）。

### 6.3 存储与降采样

**全部数据在一个 SQLite 文件里**（纯 Go 驱动 `modernc.org/sqlite`，无 CGO），方便备份和迁移。

- WAL 模式：运行中旁边会有 `probe.db-wal` / `-shm`，里面可能有已提交的数据。正常停止时执行 `wal_checkpoint(TRUNCATE)` 并删除它们，停机后只剩 `probe.db` 一个文件。
- 一个专用写连接（ingest、降采样、清理都走它），另开只读连接池给 Web 查询，避免 `SQLITE_BUSY`。
- 节点 id 映射为整数（`nodes` 表），各数据表用 `WITHOUT ROWID` + 复合主键，减少体积。
- schema 有版本号（`meta.schema_version`，当前为 2），升级按版本逐步执行，只加表不删数据；遇到比程序更新的版本拒绝打开。

| 表 | 主键 | 内容 | 保留 |
|---|---|---|---|
| `metrics_raw` / `_5m` / `_1h` | (node, ts) | cpu、steal、load、mem、swap；聚合表存 avg 与 max | 48h / 30d / 400d |
| `net_raw` / `_5m` / `_1h` | (node, iface, ts) | 各网卡收/发速率 | 同上 |
| `disk_raw` / `_1h` | (node, mount, ts) | 用量、inode | 48h / 400d |
| `ping_raw` / `_5m` / `_1h` | (src, dst, ts) | sent、lost、min/avg/max、jitter | 同 metrics |
| `traffic_period` | (node, iface, period_start) | rx、tx、ts（latest-ts-wins） | 永久 |
| `traffic_daily` | (node, iface, day) | 当日最后看到的周期总量 | 永久 |
| `node_status` | node | 最大报文 ts、最后"新鲜"到达时间、系统信息 | — |
| `alert_state` | (rule, node, target) | firing 状态、流量已提醒的档位（target 为挂载点 / peer 名 / 周期起始） | 规则删除时清理 |
| `alert_history` | id（按 ts 索引） | 告警、重复提醒、恢复、流量档位事件及消息原文 | 跟随 `retention.h1` |

写入规则：

- 一份报文可能拆成多个包，共享同一 `ts`。`metrics_raw` 用 `ON CONFLICT DO UPDATE SET col = COALESCE(excluded.col, col)` 合并；其他表的行天然由各自主键区分。全部写入都是幂等的，重复包不会产生重复数据。
- **在线判断**：只有 `ts` 大于该节点已见最大 `ts` 的报文才更新"最后新鲜到达时间"；在线 = 该时间在 `offline_after`（默认 30s）内。重放的旧包不会让离线节点显示在线；agent 时钟偏快也不会让它在死后一直显示在线。这两个值持久化在 `node_status`，服务端重启后恢复。
- 服务端内存里保留近 2h 已处理的包 id：重放/重传的包直接回 ACK，不再写库。

降采样与清理：

- 聚合一律按报文 `ts` 分桶，与到达时间无关。
- 每分钟重算最近 3h 内的 5m、1h 桶（agent 补传最多晚到 2h）；服务端启动时重算整个 raw 保留窗口，弥补停机期间没算的桶。重算用 `INSERT OR REPLACE`，天然幂等。
- 每小时清理过期数据。
- 估算：10 节点 × 各 10 个 peer，ping 是大头：raw 约 170 万行、5m 约 86 万行、1h 约 96 万行，总体积在几百 MB 以内。

### 6.3.1 备份与迁移

- `vps-probe-server backup -o <file>`：运行中也能用（`VACUUM INTO`），得到一个一致的单文件。
- 服务端每天自动备份一份到 `backup.dir`，保留 `backup.keep` 份。
- **迁移只需要两个文件**：`server.yml`（含各节点 token）+ `probe.db`（或 `backup` 产生的文件）。手动复制时先 `systemctl stop`，确认 `probe.db` 旁边没有 `-wal`/`-shm` 文件再拷贝。
- 迁移后服务端 IP 变化，需要改各 agent 的 `server.addr`；若写的是域名，改 DNS 即可（agent 断线期间每 60s 重新解析）。

### 6.4 Web 只读 API

| 路径 | 内容 |
|---|---|
| `GET /api/nodes` | 节点列表 + 最新状态 |
| `GET /api/nodes/{id}/metrics?from&to` | 自动按时间跨度选择 raw/1m/5m |
| `GET /api/nodes/{id}/disks?from&to` | 磁盘用量 |
| `GET /api/nodes/{id}/net?from&to` | 各网卡速率 |
| `GET /api/ping/matrix?window=5m` | N×N 最近窗口的 avg/loss |
| `GET /api/ping/{src}/{dst}?from&to` | 单条链路的历史 |
| `GET /api/traffic?periods=N` | 各节点最近 N 个周期的总量（默认 24，含各网卡明细） |
| `GET /api/traffic/{id}/daily?period=...` | 某节点周期内每日流量 |
| `GET /api/alerts?from&to` | 当前告警、告警历史（默认最近 7 天）、生效的规则 |

- 按时间跨度自动选表：≤ 6h 用 raw，≤ 7d 用 5m，更长用 1h；再在 SQL 里按 `ts / step` 分组，每条曲线最多约 1000 个点。
- ping 矩阵的列是各 peer 的 `name`；与节点 id 不一致的 name 也会显示为单独一列。
- `GET /api/stats`：ingest 各丢弃原因的计数、数据库大小。

所有接口都是 GET，服务端不注册任何写接口。

## 7. 告警

规则示例：

```yaml
alerts:
  - name: cpu_high
    metric: cpu            # cpu | mem | swap | disk | load1 | steal | offline | ping_loss | ping_avg | traffic
    op: ">"
    threshold: 90
    for: 5m                # 持续多久才触发
    nodes: all             # 或 [hk-1, jp-1]
    repeat: 1h             # 仍未恢复时的重复提醒间隔，0 表示不重复
    notify_recovery: true
  - name: disk_full
    metric: disk
    op: ">"
    threshold: 85
    for: 10m
  - name: offline
    metric: offline
    for: 60s               # 超过 60s 未上报
  - name: link_loss
    metric: ping_loss      # 按 (src, dst) 链路评估
    op: ">"
    threshold: 20
    for: 3m
  - name: traffic_quota
    metric: traffic        # 按配额百分比，每个周期每个档位只提醒一次
    levels: [80, 90, 100]
```

- 状态机：`ok → pending（满足条件但未达到 for）→ firing → ok`；进入 firing 和恢复时各发一条 TG 消息。
- **防抖**：firing 之后，条件要连续 `min(for, 1m)` 不满足才算恢复，避免在阈值附近来回跳时反复告警/恢复。
- 每 10s 评估一次，基于最新数据：
  - cpu、steal、load1、mem、swap 取最新一条；disk 按挂载点分别评估；
  - ping_loss、ping_avg 按 (src, dst) 链路取最近 60s 的汇总（单个 10s 样本只有 10 个包，太抖）；
  - offline = 超过 `for` 没有新鲜报文（从未上报的节点也算），不经过 pending；
  - traffic 按节点的 `traffic_quota_mode` 计算已用量，没设配额的节点跳过；状态按 (规则, 节点, 周期起始) 记录，每个周期每个档位只提醒一次，下个周期自动重新计。
- **数据缺失**（节点离线、没有对应数据）时：firing 的告警保持不动，不发恢复；pending 的归零。
- 服务端启动后的前 2 分钟不评估 offline，避免服务端重启时误报所有节点离线。
- 未配置 `alerts` 时使用默认规则：offline 60s、cpu > 90% 5m、mem > 90% 5m、disk > 90% 10m、ping_loss > 20% 3m、traffic [80, 90, 100]。写了 `alerts` 就只用写的规则。
- firing 状态和流量档位持久化在 `alert_state`，服务端重启不会重复告警；`alert_history` 保留 400 天（跟随 `retention.h1`）。配置里删掉的规则，其状态在启动时清理。

Telegram：

- 只调用 `sendMessage`，不收消息（不用 getUpdates / webhook），bot 无法向服务端下指令。
- 纯文本发送（不用 parse_mode），agent 上报的字符串无法注入格式。
- 同一评估轮次的消息合并成一条，超过 4096 字符拆分；发送失败按指数退避重试，队列有上限。
- 请求失败的错误信息里会带 URL（含 bot token），记录日志前脱敏。
- 未配置 telegram 时，告警照常评估和记录，消息内容写入服务端日志。
- `vps-probe-server test-telegram` 发送一条测试消息，用于检查配置。
- 消息里的时间按配置的 `timezone` 显示。

## 8. 前端页面

1. **总览**：每个节点一张卡片，显示在线状态、CPU/内存/磁盘进度条、实时网速、本周期流量/配额进度、运行时长。
2. **节点详情**：时间范围可选 1h / 6h / 24h / 7d / 30d / 自定义；图表包括 CPU（含 steal）、负载、内存/Swap、磁盘、各网卡速率，以及该节点到各 peer 的时延。
3. **时延矩阵**：N×N 热力图（颜色表示 avg，角标表示丢包），点击格子查看该链路的历史曲线（min/avg/max 区间带 + 丢包柱）。
4. **流量**：各节点本周期/历史周期的收、发、合计与配额表格，以及本周期每日流量柱状图。
5. **告警**：告警历史列表。

- 页面自动适配深色/浅色模式，移动端可用（卡片在窄屏下单列，矩阵表格可横向滚动）。
- 实时刷新采用 10s 轮询（时间范围超过 24h 时 60s），不使用 WebSocket，保持服务端简单。

实现方式：

- 原生 JS 单页应用，无构建步骤；hash 路由（`#/`、`#/node/{id}`、`#/ping`、`#/ping/{src}/{dst}`、`#/traffic`、`#/alerts`）。
- 页面文件与 ECharts（版本号写在文件名里）用 `go:embed` 打进服务端二进制，与 API 同一端口：`GET /` 返回 `index.html`，`GET /static/...` 返回静态文件，其他路径 404。静态文件长缓存 + 启动时预压缩 gzip，`/api/` 不缓存。
- 严格 CSP：`default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'`，禁止内联脚本和样式。ECharts 提示框用 `renderMode: 'richText'`（画在 canvas 上），不需要放开内联样式。
- **agent 上报的字符串（主机名、网卡、挂载点、peer 名）一律按文本插入 DOM**，不拼 HTML；进入 URL 时做 `encodeURIComponent`。被攻破的 agent 也无法借此在页面里执行脚本。
- 网速用字节/秒显示（KB/s、MB/s）；流量配额的 GB 按 1024³ 字节计算。
- 时间按浏览器本地时区显示；按日流量的日期边界来自服务端 `timezone`。

## 9. 目录结构

```
cmd/
  vps-probe-agent/      main.go（含 -dry-run）
  vps-probe-server/     main.go（serve / check / backup / test-telegram / agent-config / gen-token）
proto/probe/v1/         probe.proto
internal/
  proto/probev1/        生成的 protobuf 代码（已提交）
  wire/                 加密包格式，agent 与 server 共用
  agent/
    agent.go            采样主循环
    config/
    collect/            cpu、mem、load、disk、net、sys 采集
    traffic/            流量累计与持久化（§4）
    ping/               ICMP 互测
    report/             拆包、UDP 发送、ACK、补传
  server/
    config/             服务端配置、权限检查
    store/              SQLite：写入、降采样、清理、查询、备份
    ingest/             UDP 接收、校验、去重、ACK
    api/                只读 HTTP API
    alert/              告警规则评估、状态机
    notify/             Telegram 发送（只发不收）
    cfaccess/           Cloudflare Access JWT 校验
web/                    web.go（go:embed）+ static/（index.html、app.js、style.css、vendor/echarts）
deploy/
  vps-probe-agent.service
  agent.example.yml
  vps-probe-server.service
  server.example.yml
  cloudflared.example.yml   本地配置方式的隧道示例
  install.sh              安装 / 升级 / 卸载（agent、server）
VERSION                   版本号；`make dist` 生成发布包
docs/
  DESIGN.md
```

## 10. 测试重点

- `traffic`：重启（boot_id 变化）、计数器回绕/归零、跨月、`reset_day` = 29/30/31 遇到小月、状态文件损坏（先备份再重建，并告警）、并发落盘，全部做表驱动单元测试；采集源通过接口注入以便模拟。
- 周期计算：时区与 DST（虽然 Asia/Shanghai 没有夏令时，仍按通用逻辑实现并测试）。
- 协议：加解密往返、篡改头部/密文/附加数据、错误密钥、截断包、ACK 反射成 Report、超长包拆分。
- ingest：未知节点、解密失败、畸形 protobuf、`ts` 越界，均需静默丢弃；重复报文的幂等性。
- agent 发送器：ACK 移出队列、超时重发、服务端离线时只发探测包、队列上限。
- 告警状态机：pending/firing/恢复/重复提醒、服务端启动静默期。
- 端到端：本地启动 server + 2 个 agent（peer 互指 127.0.0.1），跑通整条链路。

## 11. 里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| M1 | agent 采集 + 流量累计 + ICMP + UDP 上报协议；`-dry-run` 模式把报文打印到 stdout | 单元测试通过；在一台 VPS 上重启后流量数据连续 |
| M2 | server UDP ingest + SQLite + 降采样 + 只读 API | 两台 agent 上报，API 能查到数据 |
| M3 | 前端五个页面 | 在浏览器里查看 |
| M4 | 告警 + Telegram | 人为制造 CPU 高负载、停掉 agent，能收到告警和恢复通知 |
| M5 | 安装脚本、systemd 加固、cloudflared 示例、README | 按 README 在一台干净的 VPS 上从零部署成功 |

## 12. 暂不做

- TCP/UDP 时延（后续可做 TCP connect 到对端已有端口）
- 容器型 VPS（OpenVZ/LXC 的 venet 网卡识别）
- 多用户 / 权限体系（依赖 CF Access）
- agent 自动更新（安全原则 2）
