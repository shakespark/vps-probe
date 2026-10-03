# vps-probe

给自己的一小群 VPS 用的探针：资源监控、节点间时延矩阵、按计费周期统计的流量（重启不丢）、DDoS 识别、到期提醒，Telegram / webhook 告警。

设计上只有一条硬规矩：**agent 只推不收，不接受服务端的任何指令。** 服务端被攻破，攻击者也没法在你的 VPS 上执行任何东西。

**在线演示**：<https://vps-probe-demo.shakespark.workers.dev>（界面是真的，节点和数据都是虚构的，在浏览器里生成，没有后端）。

| 总览 | 节点详情 |
|---|---|
| ![总览](docs/img/overview.png) | ![节点详情](docs/img/node.png) |
| **时延矩阵与链路可用性** | **告警** |
| ![时延](docs/img/ping.png) | ![告警](docs/img/alerts.png) |

## 功能

- **资源**：CPU（含 steal、软中断）、负载、内存 / Swap、磁盘、各网卡网速和包速率、TCP / UDP 连接数；10 秒一个点，保留 48 小时原始数据、30 天 5 分钟聚合、400 天 1 小时聚合。
- **流量**：按每台机器自己的计费周期（重置日、重置时刻）累计，agent 重启、VPS 重启、计数器归零都不丢；配额进度、每日用量、周期结算和每周汇总。
- **时延**：节点两两互 ping 的矩阵、每条链路的历史曲线（按丢包着色）和 24 小时 / 7 天 / 30 天可用率；还能测经隧道（端口转发、VPN）的时延。
- **告警**：离线、CPU / 内存 / 磁盘、链路丢包、流量配额档位、到期提醒、上报 IP 变化；**DDoS 识别**（入站远大于出站，附包速率和其他节点到它的丢包作为证据）和被利用对外攻击的识别。Telegram 和任意 webhook（Bark、ntfy、Discord、Server 酱……）。
- **部署**：服务端一个程序加一个 SQLite 文件，迁移只要两个文件；agent 一个 5 MB 的静态程序，普通用户运行。一条命令加节点，一条命令在 VPS 上装好 agent。
- 深色 / 浅色自适应，手机可用。只支持 Linux + systemd（amd64 / arm64）。在 KVM 的 VPS 上长期运行过；LXC 容器从 0.1.21 起支持（在 Proxmox 的 LXC 上实测过）；OpenVZ 只有网卡识别的兜底，还没有在真机上验证。

## 安全模型

写这个探针的起因，是别的探针出过"面板被攻破 → 所有被监控的机器被批量执行命令"的事故。所以这里把"面板能控制机器"这件事从设计上去掉了：

| 如果这个被攻破 | 攻击者能做的 | 做不到的 |
|---|---|---|
| **服务端** | 看到、篡改监控数据；用你的通知渠道发消息 | 在任何 VPS 上执行命令、下发配置、推送更新：agent 没有接收这些东西的代码 |
| **某一台 VPS** | 伪造这一台自己的监控数据（每台的 token 各不相同） | 冒充别的节点；借主机名之类的字符串在网页里执行脚本 |
| **GitHub 账号或 CI** | 往仓库推代码、替换 Release 里的文件 | 发出能通过验证的发布包：签名密钥离线保存，CI 不持有；构建可复现，任何人都能核对 |
| 没登录的访客 | 无 | 网页必须先登录（Cloudflare Access 或 Basic 认证），没有匿名可见的页面 |

具体做法：

- agent 以普通用户运行，**不监听任何端口**，只向服务端发加密的 UDP 报文；唯一收的是"已收到"的确认。
- agent 不执行命令、不下载脚本、不自动更新、不从服务端拉配置；ping 谁也写在它自己的配置文件里。
- 网页**只读**，只有 GET 接口；所有配置都在服务端的配置文件里，改配置要登录服务器。
- 通知渠道只发不收，没有"给 bot 发消息来控制探针"这回事。
- 服务端唯一对公网开放的是一个 UDP 端口，校验不过的包一律不回应，端口扫描看起来和关着一样。

完整的设计和取舍见 [docs/DESIGN.md](docs/DESIGN.md)。

## 和常见探针的区别

以"带远程管理功能的探针"（哪吒这一类）的默认配置为参照：

| | vps-probe | 带远程管理的探针 |
|---|---|---|
| 面板向 agent 下发命令 / 网页终端 / 计划任务 | 没有，设计上排除 | 有 |
| agent 权限 | 普通用户，不监听端口 | 通常是 root |
| agent 自动更新 | 没有，升级由你在 VPS 上执行 | 通常默认开启 |
| 配置入口 | 配置文件和命令行 | 网页后台 |
| 网页 | 只读，必须登录 | 可公开展示，可在网页上管理 |
| 发布包 | 离线密钥签名，可复现构建 | 各项目不同 |

相应地，**这些它不做**，需要的话请用别的探针：

- 网页上加节点、改配置；网页终端、远程执行命令、文件管理。
- 公开的状态页（给别人看的那种）。
- Windows / macOS / 非 systemd 系统的 agent。
- 对网站或端口的可用性监控（HTTP / TCP 探测）；它只测节点之间的时延。
- 多用户和权限。

## 快速开始

1. **服务端**：下载发布包并验证签名（第 1 步），`./install.sh server` 安装，放行 UDP 9527（第 2 步）。
2. **网页**：用 Cloudflare Tunnel + Access，或者 Caddy 加 Basic 认证（第 3 步）。
3. **每台 VPS**：服务端上 `vps-probe-server add-node -id hk-1 ...`，重启服务端，把它打印的那一行命令粘贴到 VPS 上（第 4 步）。
4. **告警**：填 Telegram 或 webhook，`vps-probe-server test-notify`（第 5 步）。

组成：

- agent：每台 VPS 一个，普通用户运行，不监听任何端口，通过加密 UDP 上报。
- 服务端：一台公网 VPS，UDP 9527 收上报；网页只监听本机 8080，登录后才能访问。
- 全部数据在一个 SQLite 文件里；迁移只需 `server.yml` + `probe.db` 两个文件。

下文命令都以 root 执行。

## 1. 获取发布包

每个发布包都包含三个程序、`install.sh`、systemd 服务文件、示例配置和许可证文件；同一个包既能装服务端也能装 agent。VPS 是 ARM 的就用 arm64 包（`uname -m` 显示 `aarch64`）。

从 [GitHub Releases](https://github.com/shakespark/vps-probe/releases) 下载，连同同一版本的 `.sha256` 和 `.sha256.sig`：

```sh
V=0.1.21
B=https://github.com/shakespark/vps-probe/releases/download/v$V
curl -fLO $B/vps-probe-$V-linux-amd64.tar.gz -fLO $B/vps-probe-$V.sha256 -fLO $B/vps-probe-$V.sha256.sig
```

**验证签名。** 每个版本都由维护者用离线密钥签名（CI 不持有密钥，见 [docs/DESIGN.md](docs/DESIGN.md) §13）。发布公钥（指纹 `SHA256:58Ji7UsUqJ+HoARS8RJsg88YH+M1bhivbOcJSXY/0Dg`）：

```
shakespark namespaces="vps-probe-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINDQDD9r2Mz+c1/6CsxfhDCaC36NKng8VLjgct61j567
```

```sh
echo 'shakespark namespaces="vps-probe-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINDQDD9r2Mz+c1/6CsxfhDCaC36NKng8VLjgct61j567' > release-signers
ssh-keygen -Y verify -f release-signers -I shakespark -n vps-probe-release \
    -s vps-probe-$V.sha256.sig < vps-probe-$V.sha256    # 应输出 Good "vps-probe-release" signature
sha256sum -c --ignore-missing vps-probe-$V.sha256      # 应输出 OK
```

公钥以本仓库的 `internal/release/release-signers` 为准，以后升级沿用同一个。需要 OpenSSH ≥ 8.1（Debian 11、Ubuntu 20.04 起自带）。

**自己构建**也可以，需要 Go 1.24+：

```sh
make test     # go vet + 单元测试
make dist     # dist/vps-probe-<版本>-linux-{amd64,arm64}.tar.gz 及 .sha256
```

版本号在 `VERSION` 文件里。构建是可复现的：检出某个 tag 构建出的包与该版本 Release 里的逐字节一致（Makefile 自动使用 go.mod 指定的 Go 版本，本机版本不同时会下载）。

## 2. 安装服务端

把发布包传到服务端 VPS（或在 VPS 上按第 1 步直接下载并验证）并解压：

```sh
scp vps-probe-0.1.0-linux-amd64.tar.gz root@服务端IP:
ssh root@服务端IP
tar xzf vps-probe-0.1.0-linux-amd64.tar.gz && cd vps-probe-0.1.0-linux-amd64
sha256sum -c SHA256SUMS
```

写配置：以 `examples/server.example.yml` 为模板，每个节点用 `./bin/vps-probe-server gen-token` 生成一个 token：

```sh
cp examples/server.example.yml server.yml
./bin/vps-probe-server gen-token      # 每个节点执行一次
vi server.yml                          # 填 nodes（id、name、token、addr），其余按需
./install.sh server --config server.yml
rm server.yml                          # 已安装到 /etc/vps-probe/server.yml
```

`install.sh` 会创建系统用户 `vps-probe-server`、安装程序和 systemd 服务、把配置装成 `root:vps-probe-server 0640`，先校验配置再启动。不带 `--config` 时会装一份示例配置并停下，提示你编辑后再运行一次。

节点里的 `addr` 是其他节点 ping 它用的公网地址（IP 或域名），只用于第 4 步生成 agent 配置。

**放行 UDP 9527**（云厂商安全组 / 防火墙），同时监听 IPv4 和 IPv6。非法包一律静默丢弃，端口对扫描器看起来是关着的。

## 3. 访问网页

网页只监听 `127.0.0.1:8080`，必须先登录才能看。两种方式选一种：

- **A. Cloudflare Tunnel + Access**（推荐）：不用开任何入站端口，登录由 Cloudflare 负责。
- **B. HTTPS 反向代理 + Basic 认证**：不用 Cloudflare，需要一个域名和 80/443 端口。

已经有自带登录的反向代理（Authelia、oauth2-proxy、Tailscale 等）的，把它指向 `127.0.0.1:8080` 即可，两种都不用配。

### A. Cloudflare Tunnel + Access

靠 Cloudflare Access 挡在前面；配置 `cf_access` 后服务端还会自己校验 Access 签发的令牌，隧道之外的请求一律 403。

按这个顺序做，不会把自己锁在外面：

1. **添加公网主机名**。在 Zero Trust 面板 → Networks → Tunnels → 选服务端上运行的隧道 → Public hostname → Add：
   - Subdomain / Domain：例如 `probe.example.com`
   - Service：`HTTP`，URL：`localhost:8080`

   本地配置文件方式的隧道见 `examples/cloudflared.example.yml`。
2. **加 Access 保护**。Zero Trust → Access → Applications → Add an application → Self-hosted：
   - Application domain 填上面的主机名；
   - Policy：Action = Allow，Include = Emails，填你的邮箱（登录方式用默认的邮箱验证码即可）。
3. **从外面验证**：`curl -sI https://probe.example.com/` 应返回 `302`，跳转到 `*.cloudflareaccess.com`。浏览器打开能看到登录页，登录后能看到探针页面。
4. **让服务端也校验 Access**。在 `/etc/vps-probe/server.yml` 加：

   ```yaml
   cf_access:
     team_domain: myteam.cloudflareaccess.com   # Zero Trust → Settings → Custom Pages 里的 Team domain
     aud: "<64 位十六进制>"                      # Access → Applications → 该应用 → Overview → Application Audience (AUD) Tag
   ```

   然后 `systemctl restart vps-probe-server`，再用浏览器确认能正常访问。之后直接访问 `http://服务端IP:8080` 或在服务器上 `curl 127.0.0.1:8080` 都会得到 403，这是预期行为。

### B. HTTPS 反向代理 + Basic 认证

需要服务端 ≥ 0.1.16。浏览器会弹出用户名密码框；密码随每个请求发送，所以**必须走 HTTPS**，`listen.web` 保持 `127.0.0.1:8080` 不要改。

1. **生成密码哈希**（配置里只存哈希，至少 10 个字符）：

   ```sh
   vps-probe-server hash-password        # 输入两次密码，不回显；打印 $2a$12$... 一行
   ```

2. **写进 `/etc/vps-probe/server.yml`** 并重启：

   ```yaml
   basic_auth:
     user: admin
     password_hash: "$2a$12$..."          # 上一步打印的那一行，要加引号
   ```

   ```sh
   vps-probe-server check -config /etc/vps-probe/server.yml && systemctl restart vps-probe-server
   ```

3. **反向代理**。用 [Caddy](https://caddyserver.com/) 最省事，证书自动申请，`/etc/caddy/Caddyfile`：

   ```
   probe.example.com {
       reverse_proxy 127.0.0.1:8080
   }
   ```

   nginx 等其他反向代理也可以，要求：和服务端装在同一台机器上，转发到 `127.0.0.1:8080`；带上 `X-Forwarded-For`（nginx：`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`）。服务端按这个头对输错密码的来源限速，只信任来自本机的连接带的这个头；反向代理在别的机器上时，所有访客会被算成同一个来源。

4. **验证**：`curl -sI https://probe.example.com/` 返回 `401`；浏览器打开，输入用户名密码后看到探针页面。

说明：

- 同一来源连续输错 5 次后，要等一会儿才能再试（之后每 12 秒 1 次），期间返回 429；已经登录的浏览器不受影响。
- 没有"退出登录"按钮，关闭浏览器即可。改密码：重新 `hash-password`，替换配置并重启。
- `basic_auth` 和 `cf_access` 不能同时配置。

## 4. 添加节点（每台 VPS）

需要服务端 ≥ 0.1.17，并且 VPS 能访问 GitHub（或你指定的镜像）。先在 `/etc/vps-probe/server.yml` 里写一次 agent 上报用的地址，以后的命令就不用再带 `-server`：

```yaml
server_addr: probe.example.com:9527     # 服务端的域名或 IP + UDP 端口
```

**第一步，在服务端上**登记节点（生成 token 并写进 `server.yml`，原文件留一份 `.bak-时间`）：

```sh
vps-probe-server add-node -id hk-1 -name "香港 1" -addr hk1.example.com -region HK
systemctl restart vps-probe-server
```

- `-addr` 是其他节点 ping 它用的公网地址（IP 或域名）；不写就没有节点 ping 它（家里的机器、NAT 机）。
- `-name`、`-region`、`-group` 可选。配额、到期日等其他字段照旧手工编辑 `server.yml`。
- `add-node` 只在文件里插入这一个节点，其余内容（注释、格式）一个字都不动；插入后的配置通不过校验时不会改文件。

**第二步，在这台 VPS 上**以 root 粘贴 `add-node` 打印出的那一行命令。它会：下载与服务端同版本的发布包 → 用发布公钥验证签名和校验和 → 写入这个节点的配置 → 安装并启动 agent。看到 `agent is reporting: the server acknowledged it` 就成功了。

- 这行命令里有该节点的 token，别贴到聊天或工单里。粘贴执行后它会留在这台 VPS 的 root shell 历史里；token 本来就保存在同一台机器的 `/etc/vps-probe/agent.yml`（也只有 root 能读），所以没有多暴露什么。
- 不想留历史：在服务端（或能同时登录两边的电脑）上直接 `vps-probe-server install-cmd -node hk-1 | ssh root@那台VPS sh`，命令不经过任何终端和历史。
- 重启服务端后尽快装 agent：从没上报过的新节点约 2 分钟后会触发一次离线告警。

**以后再要这行命令**（重装、对端列表变了）。它每次都用服务端生成的配置覆盖那台 VPS 上的 `agent.yml`（旧的留一份 `.bak-<时间>`）：

```sh
vps-probe-server install-cmd -node hk-1
```

- 新节点加了 `-addr` 后，想让已有节点也 ping 它：对每个已有节点执行一次 `install-cmd`，把打印的命令在那台 VPS 上运行（`add-node` 的输出里列出了涉及的节点）。
- 升级全部 agent：服务端升级后用 `vps-probe-server install-cmd -upgrade`。它打印的命令**只换程序、不动配置**，里面没有 token，所有节点用同一条；装的是与服务端相同的版本（`-version` 可指定别的版本，须 ≥ 0.1.19）。手工改过 `agent.yml` 的节点（比如指定了 `interfaces`）也用它。那台机器上还没装 agent 时它会拒绝执行。

  ```sh
  for h in vps1 vps2 vps3; do vps-probe-server install-cmd -upgrade | ssh root@$h sh; done
  ```
- 下载有时限（连接 20 秒，连续 30 秒几乎没有数据就放弃），连不上会报错退出而不是一直等。GitHub 访问不了时用 `-base https://你的镜像/路径`，镜像上放 `v<版本>/` 目录和发布页里的三个文件即可；镜像不需要可信，签名照样验证。
- VPS 上需要 OpenSSH ≥ 8.1（Debian 11、Ubuntu 20.04 起自带）和 curl 或 wget。

<details>
<summary>手动方式（不联网下载，或想自己检查配置文件）</summary>

在**服务端**上：

```sh
vps-probe-server gen-token                 # 新节点的 token
vi /etc/vps-probe/server.yml               # nodes 里加一项：id、name、token、addr
vps-probe-server check -config /etc/vps-probe/server.yml
systemctl restart vps-probe-server
vps-probe-server agent-config -node hk-1 -server 服务端IP:9527 -o /root/hk-1.yml
```

`agent-config` 生成该节点的 `agent.yml`（权限 0600）：token、服务端地址，以及所有写了 `addr` 的其他节点作为 ping 对象。

在**这台 VPS** 上：

```sh
scp root@服务端IP:/root/hk-1.yml .          # 或者用你自己的方式把文件拷过来
tar xzf vps-probe-0.1.0-linux-amd64.tar.gz && cd vps-probe-0.1.0-linux-amd64
./install.sh agent --config ../hk-1.yml
rm ../hk-1.yml                               # 服务端上的 /root/hk-1.yml 也删掉
```

</details>

无论哪种方式，生成的都只是一个本地配置文件：**agent 不会从服务端拉取任何东西**。

- 如果系统的 `net.ipv4.ping_group_range` 不允许普通用户 ping，`install.sh` 会只给 agent 服务加 `CAP_NET_RAW`（`/etc/systemd/system/vps-probe-agent.service.d/icmp.conf`），不改系统设置。
- LXC 容器（agent ≥ 0.1.21）：`install.sh` 会给 agent 服务加一个 drop-in（`vps-probe-agent.service.d/lxcfs.conf`），关掉三项会换掉 `/proc` 的沙箱设置，否则上报的是宿主机的内存和 CPU。负载和 CPU 使用率以容器里 `uptime`、`vmstat` 看到的为准，很多商家的容器里它们反映的是整台宿主机或整个 CPU 核。宿主机的 lxcfs 出故障时（容器里 `free` 报 `Transport endpoint is not connected`），agent 照常上报流量、磁盘和时延，CPU、内存、负载显示「—」，重启容器即可恢复。从 0.1.18–0.1.20 升级的 LXC 节点，升级那一次会把容器启动以来的流量多计一遍，介意就先重启容器再升级。
- 默认自动识别物理网卡（有 `/sys/class/net/<网卡>/device` 的）；OpenVZ、LXC 这类没有物理网卡的容器，改为统计默认路由所在的网卡（agent ≥ 0.1.18）。有两块以上物理网卡时只统计带默认路由的那几块，内网网卡不计入（agent ≥ 0.1.20）；默认路由走隧道（WireGuard、WARP 等）的机器仍然统计全部物理网卡。想自己指定就在 agent.yml 里写 `interfaces: [eth0]`：比如第二块网卡也走公网计费流量、但上面没有默认路由。
- 某些节点之间不想互 ping：在其中一方写 `no_ping: [对方 id, ...]`（双向生效），然后给涉及的节点重新生成 agent.yml 并安装。
- 测经隧道的时延：在发起探测的节点下写 `extra_peers`，再重新生成它的 agent.yml。
  - VPN 型隧道：`{ name: cf-vpn, addr: 1.1.1.1 }`，到目标的路由要走隧道。
  - TCP/UDP 转发型隧道（realm 等），两种方式（需 agent ≥ 0.1.4）：
    - **隧道回显（推荐）**：隧道远端指向某台机器的 `39527/udp`，在那台机器上 `./install.sh echo --config echo.yml`（`key` 用 `gen-token` 生成，见 `examples/echo.example.yml`），探测方写 `{ name: hk2-via-sgp, addr: "127.0.0.1:本地转发端口", type: echo, key: 同一个 key }`。测的是隧道本身，不经任何公共服务。
    - **DNS**：隧道远端指向 `1.1.1.1:53`，写 `{ name: cf-relay, addr: "127.0.0.1:本地转发端口", type: dns }`。
  - 本地转发规则只监听 `127.0.0.1`；转发到 DNS 的端口如果监听 `0.0.0.0`，就成了公网开放的 DNS 中继。
- 服务端那台机器本身也可以装 agent，`-server` 写 `127.0.0.1:9527` 即可。
- 可选：在节点下写 `region`（地区代码，如 `HK`、`US-LA`，显示为角标）和 `group`（总览按分组切换）；总览默认顺序就是配置文件里的顺序。只改服务端配置。
- 连接数（TCP / UDP / TIME_WAIT）和线程数需 agent ≥ 0.1.9，旧 agent 显示「—」。
- 可选：在节点下写 `expire_at`（到期日）、`renew_months`（自动续费周期，月）、`price`（显示用），总览会显示剩余天数，默认的 `expiry` 规则在到期前 7 天和 1 天提醒（写法见 `examples/server.example.yml`）。只改服务端配置，重启服务端即可。

## 5. 告警与通知

- 规则写在 `server.yml` 的 `alerts` 里（示例见 `examples/server.example.yml`，完整说明见 `docs/DESIGN.md` §7）。不写则用默认规则：离线 60s、CPU/内存 > 90% 持续 5 分钟、磁盘 > 90% 持续 10 分钟、链路丢包 > 20% 持续 3 分钟、流量配额 80/90/100%、到期前 7/1 天、DDoS（入站）和对外攻击（出站）、流量周期结算和每周流量汇总。已经写了 `alerts` 的，要到期提醒需自己加上 `expiry` 规则。
- DDoS 识别（服务端 ≥ 0.1.10，不需要升级 agent）：`metric: net_in` / `net_out` 按最近 60s 平均网速（Mbps）告警，加 `ratio` 要求本方向至少是反方向的几倍。中转机正常流量收发对称，被打时入站远大于出站，被利用去打别人时出站远大于入站。默认规则 `ddos`（入站 ≥ 50 Mbps 且 ≥ 4 倍出站，持续 2 分钟）和 `abuse_out`（出站同理，5 分钟）；以下载为主或做种的节点用 `exclude: [节点 id]` 排除（除这些之外的全部节点，以后新加的节点自动包含）。被打后遭商家黑洞时，离线告警会附上停止上报前的入站峰值。已经写了 `alerts` 的要自己加这两条，写法见 `deploy/server.example.yml`。
- 包速率和软中断（agent ≥ 0.1.14）：节点详情多一张「包速率」图，CPU 图多一条软中断线；告警可用 `metric: pps_in` / `pps_out`（包/秒）和 `metric: softirq`（%）。用来发现 SYN flood 这类字节速率不高、包数很多的攻击。不在默认规则里，阈值请先看几天包速率图再定。
- 流量报告（服务端 ≥ 0.1.11）：`metric: period_report` 在每个节点的流量周期结束时发一份结算（上下行、配额使用率、日均、最多的一天）；`metric: weekly_report` 每周在 `at`（如 `"Mon 09:00"`）发一条所有节点的汇总：本周期已用、近 7 天、按近 7 天速率推算的周期末用量（可能超额时标 ⚠️）、下次重置日，以及 30 天内到期的节点。推算要求服务端配置里节点的 `reset_day` / `reset_time` 与 agent 一致。
- 上报来源 IP 变化通知（`metric: ip_change`）不在默认规则里，需要时自己加，动态 IP 的节点建议用 `exclude` 排除。
- Telegram：在 @BotFather 用 `/newbot` 建一个**专用** bot，给它发一条消息，再打开 `https://api.telegram.org/bot<TOKEN>/getUpdates` 找 `"chat":{"id":` 后面的数字。填进 `telegram.bot_token` / `telegram.chat_id`，然后：

  ```sh
  runuser -u vps-probe-server -- vps-probe-server test-notify -config /etc/vps-probe/server.yml
  systemctl restart vps-probe-server
  ```

- Webhook（服务端 ≥ 0.1.18）：其他推送服务都用 `webhooks` 接，可以配多个，和 Telegram 同时发。每条告警对每个 webhook 发一个 HTTP 请求；`{{title}}` 是消息第一行，`{{message}}` 是全文。写在 `url` 里会自动做 URL 编码；写在 JSON `body` 里会自动变成带引号的 JSON 字符串，所以**模板里不要再给它加引号**。

  ```yaml
  webhooks:
    - name: bark                       # 渠道名，出现在日志和告警页
      url: "https://api.day.app/你的KEY/{{title}}/{{message}}"
      method: GET
    - name: ntfy
      url: "https://ntfy.sh/你的主题"
      headers: { Content-Type: text/plain }
      body: "{{message}}"
    - name: discord                    # Slack 同理，把 content 换成 text
      url: "https://discord.com/api/webhooks/ID/TOKEN"
      body: '{"content": {{message}}}'
    - name: serverchan                 # Server 酱
      url: "https://sctapi.ftqq.com/你的KEY.send"
      headers: { Content-Type: application/x-www-form-urlencoded }
      body: "title={{title}}&desp={{message}}"
    - name: wecom                      # 企业微信群机器人
      url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=你的KEY"
      body: '{"msgtype": "text", "text": {"content": {{message}}}}'
  ```

  `method` 默认 POST；不写 `body` 时发 `{"title": ..., "message": ...}`；`Content-Type` 默认 `application/json`。返回 2xx 算成功，5xx 和网络错误会重试，其他 4xx 不重试（看 `journalctl -u vps-probe-server`，日志里只有渠道名，不会出现带密钥的 URL）。改完 `test-notify` 再重启服务端。
- 一个渠道都没配时告警照常评估，记录在网页「告警」页，消息内容写入 `journalctl -u vps-probe-server`。

## 6. 升级

用新的发布包，在各台机器上重新运行同样的命令即可，**不带 `--config` 时保留现有配置**：

```sh
./install.sh server      # 服务端
./install.sh agent       # 每台 VPS；或者不拷贝发布包：在服务端 install-cmd -upgrade，把打印的命令在每台 VPS 上运行
./install.sh echo        # 装了隧道探测应答端的机器
```

agent 停止前会把流量最后读一次并保存，升级不会丢月流量；服务端数据库结构有变化时启动时自动升级（只加表或加列，不删数据）。**先升级服务端，再升级 agent**：新 agent 多报的字段旧服务端会忽略，但服务端配置里用了新字段时旧服务端会拒绝启动。建议升级服务端前先做一次备份（见下节）。

## 7. 备份与迁移

全部数据在一个文件 `/var/lib/vps-probe-server/probe.db` 里。

- 运行中备份，得到一个一致的单文件：

  ```sh
  runuser -u vps-probe-server -- vps-probe-server backup -config /etc/vps-probe/server.yml \
      -o /var/lib/vps-probe-server/probe-$(date +%F).db
  ```

  用服务用户执行，避免 root 在数据目录里留下服务读不了的文件。
- 服务端每天 04:00 后自动备份到 `/var/lib/vps-probe-server/backups/`，保留 7 份。
- **迁移到新机器只需要两个文件**：`/etc/vps-probe/server.yml`（含 token）和 `probe.db`（或上面的备份文件）。在新机器上：

  ```sh
  ./install.sh server --config server.yml
  systemctl stop vps-probe-server
  install -m 0600 -o vps-probe-server -g vps-probe-server probe.db /var/lib/vps-probe-server/probe.db
  rm -f /var/lib/vps-probe-server/probe.db-wal /var/lib/vps-probe-server/probe.db-shm
  systemctl start vps-probe-server
  ```

- 直接从旧机器拷 `probe.db` 时，先 `systemctl stop vps-probe-server`，确认旁边没有 `probe.db-wal` / `probe.db-shm` 再拷。
- 服务端 IP 变了要改各 agent 的 `server.addr`（重新生成 agent.yml 并 `./install.sh agent --config ...`）；如果写的是域名，改 DNS 即可。
- agent 的月流量权威数据在各 VPS 的 `/var/lib/vps-probe/traffic.json`，重装 VPS 前可以一起带走。

## 8. 卸载

```sh
./install.sh uninstall agent            # 保留配置和流量状态
./install.sh uninstall server           # 保留配置和数据库
./install.sh uninstall echo             # 隧道探测应答端
./install.sh uninstall agent --purge    # 连同配置、流量状态、系统用户一起删除
./install.sh uninstall server --purge   # 连同配置、数据库、备份、系统用户一起删除
```

服务端和 agent 装在同一台机器上时，卸载其中一个不会影响另一个。

## 9. 排查

| 现象 | 看哪里 |
|---|---|
| agent 日志一直 `no acknowledgement from server` | 服务端 `journalctl -u vps-probe-server`：`authentication failed` = token 不一致；`packet for a node not in the config` = node id 没登记；`timestamp too far` = 时钟偏差（检查 NTP）。都没有 = 包没到服务端，查安全组 / 防火墙的 UDP 9527 |
| 丢弃计数 | 网页底部；或未开 `cf_access` / `basic_auth` 时在服务端 `curl -s 127.0.0.1:8080/api/stats` |
| 网页 403 | 开了 `cf_access`：只能经 Cloudflare Access 访问；服务端日志 `cf_access: request rejected` 会写明原因 |
| 网页 401 / 429 | 开了 `basic_auth`：401 是用户名或密码不对，429 是同一来源输错太多次，等一分钟；服务端日志 `basic_auth: wrong user name or password` 带来源地址 |
| 时延矩阵里对不上 | agent 的 `ping.peers[].name` 要写对端的节点 id；用 `agent-config` 生成的配置自动满足 |
| agent 起不来：`no physical network interface and no default route detected` | 在 agent.yml 写 `interfaces: [网卡名]` |
| ping 全部丢包、日志 `ping disabled` | `install.sh` 会自动处理 ICMP 权限；手动安装时见第 4 步说明 |
| 改了 `reset_day` / `reset_time` 后流量页显示的周期不对 | 旧起始日期的周期记录还在服务端。停服务端后删掉它：`DELETE FROM traffic_period WHERE node=(SELECT id FROM nodes WHERE name='节点id') AND start='旧日期'`，`traffic_daily` 同理，再启动 |

## 附录：开发

- 不连服务端试跑 agent：

  ```sh
  cp deploy/agent.example.yml /tmp/agent.yml    # 编辑 node、token（任意 ≥32 字符）、state_dir 改成 /tmp/probe-state
  mkdir -p /tmp/probe-state
  ./dist/vps-probe-agent-linux-amd64 -config /tmp/agent.yml -dry-run
  ```

  `-dry-run` 把每份报告以 JSON 打印出来并显示加密后的包大小，不写正式的流量状态文件，可以和已安装的 agent 同时运行。
- 修改 `proto/` 后需要 `protoc` 和 `protoc-gen-go`，执行 `make proto`；生成的代码已提交。
- 目录结构和各模块说明见 `docs/DESIGN.md` §9。
- 演示站（`docs/DESIGN.md` §14）：`make demo` 生成 `dist/demo/`，是同一份界面加上在浏览器里生成假数据的 `web/demo/demo.js`，没有后端，整个目录放到静态托管的站点根路径即可（如 Cloudflare Pages：`npx wrangler pages deploy dist/demo`）。改了接口的返回字段后，`go test ./internal/server/api -update-shape` 更新 `web/demo/api-shape.json`，再改 `demo.js` 直到 `make demo-check`（需要 node）通过。
- 发布新版本（`docs/DESIGN.md` §13）：
  1. 改 `VERSION` 和本文件第 1 步里的 `V=`（CI 会检查两者一致）并提交，`git push origin main`，再 `git tag v<版本> && git push origin v<版本>`（tag 单独推送，和提交一起推可能不触发 CI）；
  2. GitHub Actions 测试、构建，建一个草稿 Release（Actions 页面看进度）；
  3. 在自己的终端执行 `make release-sign`：核对草稿里的包与本地重建逐字节一致，签名，上传签名并正式发布。只核对不签名用 `make release-verify`。签名密钥默认 `~/.ssh/vps-probe-release`，可用 `SIGNING_KEY=` 指定。
- 升级或增减 Go 依赖、更换 `web/static/vendor/` 里的前端库后执行 `make licenses`，提交更新后的 `THIRD_PARTY_LICENSES`（`make dist` 也会重新生成）。

## 许可证

[Apache License 2.0](LICENSE)。发布的程序里包含的第三方软件及其许可证见 [NOTICE](NOTICE) 和 [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES)。
