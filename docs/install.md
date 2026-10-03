# 安装与运维

[README 的快速开始](../README.md#快速开始) 是最短的路径；这里是每一步的细节和其他做法。命令都以 root 执行。

组成：

- **agent**（`vps-probe-agent`）：每台被监控的机器一个。普通用户运行，不监听任何端口，通过加密 UDP 上报。
- **服务端**（`vps-probe-server`）：一台有公网 IP 的机器。UDP 9527 收上报；网页只监听本机 8080，登录后才能看。全部数据在一个 SQLite 文件里。
- **应答端**（`vps-probe-echo`，可选）：只装在要被探测的隧道终点，见 [tunnels.md](tunnels.md)。

三个程序都在同一个发布包里，用法也一样：`run`（默认）、`check`（校验配置）、`version`。

## 1. 获取发布包

每个发布包里有三个程序、`install.sh`、systemd 服务文件、示例配置和许可证文件。从 [GitHub Releases](https://github.com/shakespark/vps-probe/releases) 下载 tar 包和同一版本的 `.sha256`、`.sha256.sig`，按 README 里的命令验证签名。ARM 的机器（`uname -m` 显示 `aarch64`）用 arm64 包。

- 每个版本都由维护者用离线密钥签名，CI 不持有密钥（见 [DESIGN.md](DESIGN.md) §10）。公钥以仓库里的 `internal/release/release-signers` 为准，以后升级沿用同一个。
- 验证需要 OpenSSH ≥ 8.1（Debian 11、Ubuntu 20.04 起自带）。
- 解压后 `sha256sum -c SHA256SUMS` 可以再核对包里的每个文件。

自己构建见 [development.md](development.md)。构建是可复现的：检出某个 tag 构建出的包和该版本 Release 里的逐字节一致。

## 2. 安装服务端

```sh
tar xzf vps-probe-<版本>-linux-amd64.tar.gz && cd vps-probe-<版本>-linux-amd64
./install.sh server
```

`install.sh` 会创建系统用户 `vps-probe-server`、安装程序和 systemd 服务、把配置装成 `/etc/vps-probe/server.yml`（`root:vps-probe-server 0640`），校验配置后启动。

- 不带 `--config` 时装的是示例配置：没有节点，其余都是默认值，服务端可以直接启动。之后编辑 `/etc/vps-probe/server.yml`，改完 `vps-probe-server check && systemctl restart vps-probe-server`。
- 已经有写好的配置：`./install.sh server --config server.yml`。
- 示例配置里每一项都有注释（发布包的 `examples/server.example.yml`）。

要做的两件事：

- **填 `public_addr`**：agent 用来上报的地址，本机的域名或公网 IP 加 `:9527`。服务端自己不用它，只写进它生成的 agent 配置里。
- **放行 UDP 9527**（云厂商安全组 / 防火墙），IPv4 和 IPv6 都监听。校验不过的包一律不回应，端口对扫描器看起来是关着的。

## 3. 访问网页

网页只监听 `127.0.0.1:8080`，必须先登录才能看。两种方式选一种：

- **A. Cloudflare Tunnel + Access**：不用开任何入站端口，登录由 Cloudflare 负责。
- **B. HTTPS 反向代理 + Basic 认证**：不用 Cloudflare，需要一个域名和 80/443 端口。

已经有自带登录的反向代理（Authelia、oauth2-proxy、Tailscale 等）的，把它指向 `127.0.0.1:8080` 即可，两种都不用配。

### A. Cloudflare Tunnel + Access

Cloudflare Access 挡在前面；配置 `cf_access` 后服务端还会自己校验 Access 签发的令牌，隧道之外的请求一律 403。按这个顺序做，不会把自己锁在外面：

1. **添加公网主机名**。Zero Trust 面板 → Networks → Tunnels → 选服务端上运行的隧道 → Public hostname → Add：主机名例如 `probe.example.com`，Service 选 `HTTP`，URL 填 `localhost:8080`。本地配置文件方式的隧道见 `examples/cloudflared.example.yml`。
2. **加 Access 保护**。Zero Trust → Access → Applications → Add an application → Self-hosted：Application domain 填上面的主机名；Policy 的 Action 选 Allow，Include 选 Emails，填你的邮箱。
3. **从外面验证**：`curl -sI https://probe.example.com/` 应返回 `302`，跳转到 `*.cloudflareaccess.com`。浏览器登录后能看到探针页面。
4. **让服务端也校验 Access**。在 `server.yml` 加：

   ```yaml
   cf_access:
     team_domain: myteam.cloudflareaccess.com   # Zero Trust → Settings → Custom Pages 里的 Team domain
     aud: "<64 位十六进制>"                      # Access → Applications → 该应用 → Overview → Application Audience (AUD) Tag
   ```

   重启服务端，再用浏览器确认能正常访问。之后在服务器上 `curl 127.0.0.1:8080` 也会得到 403，这是预期的。

### B. HTTPS 反向代理 + Basic 认证

浏览器会弹出用户名密码框。密码随每个请求发送，所以**必须走 HTTPS**，`listen.web` 保持 `127.0.0.1:8080`。

1. **生成密码哈希**（配置里只存哈希；密码至少 10 个字符）：

   ```sh
   vps-probe-server hash-password        # 输入两次密码，不回显；打印 $2a$12$... 一行
   ```

2. **写进 `server.yml`** 并重启：

   ```yaml
   basic_auth:
     user: admin
     password_hash: "$2a$12$..."          # 上一步打印的那一行，要加引号
   ```

3. **反向代理**。用 [Caddy](https://caddyserver.com/) 最省事，证书自动申请，`/etc/caddy/Caddyfile`：

   ```
   probe.example.com {
       reverse_proxy 127.0.0.1:8080
   }
   ```

   nginx 等其他反向代理也可以，要求：和服务端装在同一台机器上，转发到 `127.0.0.1:8080`，带上 `X-Forwarded-For`。服务端按这个头对输错密码的来源限速，并且只信任来自本机的连接带的这个头。

4. **验证**：`curl -sI https://probe.example.com/` 返回 `401`；浏览器输入用户名密码后看到探针页面。

说明：

- 同一来源连续输错 5 次后要等一会儿才能再试（之后每 12 秒 1 次），期间返回 429；已经登录的浏览器不受影响。
- 没有"退出登录"按钮，关闭浏览器即可。改密码：重新 `hash-password`，替换配置并重启。
- `basic_auth` 和 `cf_access` 不能同时配置。

## 4. 添加节点

**在服务端上**登记节点。它生成 token、把节点写进 `server.yml`（原文件留一份 `.bak-<时间>`），并打印一行安装命令：

```sh
vps-probe-server add-node -id hk-1 -name "香港 1" -ping-addr hk1.example.com -region HK
systemctl restart vps-probe-server
```

- `-ping-addr`：其他节点 ping 它用的公网地址（IP 或域名）。不写就没有节点 ping 它（家里的机器、NAT 后面的机器）。
- `-name`、`-region`、`-group` 可选。配额、到期日这些在 `server.yml` 里手工写，见示例配置。
- `add-node` 只在文件里插入这一个节点，其余内容（注释、格式）一个字都不动；插入后的配置通不过校验时不改文件。
- 没填 `public_addr` 时要带 `-server 服务端地址:9527`。服务端本机的 agent 用 `-server 127.0.0.1:9527`。

**在那台 VPS 上**以 root 粘贴打印出的那一行命令。它会：下载和服务端同版本的发布包 → 用发布公钥验证签名和校验和 → 写入这个节点的配置 → 安装并启动 agent。看到 `agent is reporting: the server acknowledged it` 就成功了。

- 这行命令里有该节点的 token，别贴到聊天或工单里。执行后它会留在那台 VPS 的 root shell 历史里；token 本来就在同一台机器的 `/etc/vps-probe/agent.yml` 里，没有多暴露什么。
- 不想留历史：`vps-probe-server install-cmd -node hk-1 | ssh root@那台VPS sh`。
- 重启服务端后尽快装 agent：从没上报过的新节点约 2 分钟后会触发一次离线告警。
- VPS 上需要 OpenSSH ≥ 8.1 和 curl 或 wget。下载有时限，连不上会报错退出而不是一直等。
- GitHub 访问不了时用 `-base https://你的镜像/路径`，镜像上放 `v<版本>/` 目录和发布页里的三个文件。镜像不需要可信，签名照样验证。
- 报 `mounted noexec`：那台机器的 `/tmp` 不允许执行程序，在命令前面加 `TMPDIR=/root `。

**以后再要这行命令**（重装、ping 的对象变了）：

```sh
vps-probe-server install-cmd -node hk-1
```

它每次都用服务端生成的配置覆盖那台 VPS 上的 `agent.yml`（旧的留一份 `.bak-<时间>`）。新节点加了 `-ping-addr` 后，想让已有节点也 ping 它，就对每个已有节点执行一次。

<details>
<summary>不联网下载，或者想先看看配置文件</summary>

在服务端上生成那个节点的配置：

```sh
vps-probe-server agent-config -node hk-1 -o /root/hk-1.yml
```

把这个文件和发布包拷到那台 VPS 上：

```sh
tar xzf vps-probe-<版本>-linux-amd64.tar.gz && cd vps-probe-<版本>-linux-amd64
./install.sh agent --config ../hk-1.yml
rm ../hk-1.yml                               # 服务端上的那一份也删掉
```

</details>

无论哪种方式，带过去的都只是一个本地配置文件：**agent 不会从服务端拉取任何东西**。agent 的配置各项见 `examples/agent.example.yml`。

### 网卡

流量统计默认自动识别网卡：物理网卡；有两块以上时只算带默认路由的（第二块通常是内网）；容器里没有物理网卡，算默认路由所在的那块。默认路由走隧道（WireGuard、WARP 等）的机器算全部物理网卡。

识别得不对时（比如第二块网卡也走计费流量、但上面没有默认路由），在 `server.yml` 那个节点下面写 `interfaces: [eth0, eth1]`，再用 `install-cmd -node ID` 重新安装它的配置。要多看几个挂载点的磁盘也一样，写 `disks: ["/", "/data"]`。

`install-cmd -node` 每次都按 `server.yml` 重写整个 `agent.yml`，所以这类设置要写在 `server.yml` 里；直接在节点的 `agent.yml` 里手改的内容，下次重新安装配置时会丢（只升级程序的 `install-cmd -upgrade` 不动配置）。

### LXC 容器

`install.sh` 发现是 LXC 容器时，会给 agent 服务加一个 drop-in，关掉三项会换掉 `/proc` 的沙箱设置，否则上报的是宿主机的内存和 CPU。

- 负载和 CPU 使用率以容器里 `uptime`、`vmstat` 看到的为准；很多商家的容器里它们反映的是整台宿主机或整个 CPU 核。
- 宿主机的 lxcfs 出故障时（容器里 `free` 报 `Transport endpoint is not connected`），agent 照常上报流量、磁盘和时延，CPU、内存、负载显示「—」；重启容器即可恢复。

### ICMP

系统的 `net.ipv4.ping_group_range` 不允许普通用户 ping 时，`install.sh` 只给 agent 服务加 `CAP_NET_RAW`，不改系统设置。

## 5. 升级

用新的发布包重新运行同样的命令，**不带 `--config` 时保留现有配置**。先升级服务端，再升级 agent。

```sh
./install.sh server      # 服务端
./install.sh agent       # 每台 VPS
./install.sh echo        # 装了应答端的机器
```

不想把发布包拷到每台 VPS：服务端升级后执行

```sh
vps-probe-server install-cmd -upgrade
```

它打印的命令**只换程序、不动配置**，里面没有 token，所有节点用同一条：

```sh
for h in vps1 vps2 vps3; do vps-probe-server install-cmd -upgrade | ssh root@$h sh; done
```

agent 停止前会把流量最后读一次并保存，升级不会丢流量。升级服务端前建议先备份（下一节）。

新版本需要新的数据库格式时，服务端在启动时自动升级数据库，日志里有一行 `database schema upgraded`。升级过的数据库旧版本打不开（它会报 `schema version … is newer than this build`），要退回旧版本只能用升级前的备份。0.3.0 就有这样一次升级。

### 从 0.1.x 升级

0.2 和 0.1.x 互不兼容：报文格式、数据库、两份配置文件的格式都换了。做法是重装：

1. 服务端：`./install.sh server` 换程序；按新的示例配置重写 `server.yml`（节点的 token 可以沿用）；把旧数据库移走（`mv /var/lib/vps-probe-server/probe.db{,.0.1}`），新的会自动建立。
2. 每个节点：用 `install-cmd -node ID` 打印的命令重装。agent 的系统用户和数据目录从 `vps-probe` 改成了 `vps-probe-agent`；想保留本周期已经累计的流量，先把状态文件带过去：

   ```sh
   systemctl stop vps-probe-agent
   install -d -m 0700 /var/lib/vps-probe-agent && cp /var/lib/vps-probe/traffic.json /var/lib/vps-probe-agent/
   # 安装完成后：
   chown -R vps-probe-agent: /var/lib/vps-probe-agent && systemctl restart vps-probe-agent
   userdel vps-probe; rm -rf /var/lib/vps-probe
   ```

## 6. 备份与迁移

全部数据在一个文件 `/var/lib/vps-probe-server/probe.db` 里。

- 服务端每天 04:00 后自动备份到 `/var/lib/vps-probe-server/backups/`，保留 7 份。
- 运行中手动备份，得到一个一致的单文件：

  ```sh
  runuser -u vps-probe-server -- vps-probe-server backup -o /var/lib/vps-probe-server/probe-$(date +%F).db
  ```

  用服务用户执行，避免 root 在数据目录里留下服务读不了的文件。
- **迁移到新机器只需要两个文件**：`/etc/vps-probe/server.yml`（含 token）和 `probe.db`（或上面的备份）。在新机器上：

  ```sh
  ./install.sh server --config server.yml
  systemctl stop vps-probe-server
  install -m 0600 -o vps-probe-server -g vps-probe-server probe.db /var/lib/vps-probe-server/probe.db
  rm -f /var/lib/vps-probe-server/probe.db-wal /var/lib/vps-probe-server/probe.db-shm
  systemctl start vps-probe-server
  ```

- 直接从旧机器拷 `probe.db` 时，先停服务端，确认旁边没有 `probe.db-wal` / `probe.db-shm` 再拷。
- 服务端的地址变了要改各 agent 的 `server`（重新 `install-cmd -node`）；写的是域名的话改 DNS 即可。
- 流量的权威数据在各 VPS 的 `/var/lib/vps-probe-agent/traffic.json`，重装 VPS 前可以带走。

## 7. 卸载

```sh
./install.sh uninstall agent            # 保留配置和流量状态
./install.sh uninstall server           # 保留配置和数据库
./install.sh uninstall echo
./install.sh uninstall agent --purge    # 连同配置、流量状态、系统用户一起删除
./install.sh uninstall server --purge   # 连同配置、数据库、备份、系统用户一起删除
```

装在同一台机器上的几个角色互不影响。

## 8. 排查

| 现象 | 看哪里 |
|---|---|
| agent 日志一直 `no acknowledgement from server` | 服务端 `journalctl -u vps-probe-server`：`authentication failed` = token 不一致；`packet for a node not in the config` = 节点没登记或加了以后没重启服务端；`timestamp too far` = 时钟偏差（检查 NTP）。都没有 = 包没到服务端，查安全组 / 防火墙的 UDP 9527。agent 是 0.1.x 的话，0.2 的服务端不认它的报文，也没有日志 |
| 丢弃计数 | 网页底部 |
| 网页 403 | 开了 `cf_access`：只能经 Cloudflare Access 访问；服务端日志 `cf_access: request rejected` 写明原因 |
| 网页 401 / 429 | 开了 `basic_auth`：401 是用户名或密码不对，429 是同一来源输错太多次，等一分钟 |
| 时延矩阵里对不上 | agent 的 `peers[].name` 要写对端的节点 id；服务端生成的配置自动满足 |
| agent 日志 `traffic is not counted yet` | 自动识别定不下来算哪块网卡：在 `server.yml` 那个节点下写 `interfaces: [网卡名]`，重新安装它的配置 |
| ping 全部丢包、日志 `ping disabled` | `install.sh` 会自动处理 ICMP 权限；手工安装的看上面「ICMP」 |
| 某一项显示「—」 | agent 读不到那一项，它的日志里有 `cannot read …`；LXC 里见上面「LXC 容器」 |
| 服务端启动报 `written by vps-probe 0.1.x` | 数据库是旧版本的：见「从 0.1.x 升级」 |
| 配置报 `field … not found` | 键名写错了，或者是 0.1.x 的写法；对照示例配置 |
