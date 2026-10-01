# vps-probe

自用 VPS 探针：资源监控、VPS 间 ICMP 时延、按周期统计的网卡流量（重启不丢）、Telegram 告警。

设计与安全原则见 [docs/DESIGN.md](docs/DESIGN.md)。核心一条：**agent 只推不收，不接受服务端的任何指令**，服务端被攻破也无法控制 VPS。

- agent：每台 VPS 一个，普通用户运行，不监听任何端口，通过加密 UDP 上报。
- 服务端：一台公网 VPS，UDP 9527 收上报；网页只监听本机 8080，经 Cloudflare Tunnel + Access 登录后访问。
- 全部数据在一个 SQLite 文件里；迁移只需 `server.yml` + `probe.db` 两个文件。

下文命令都以 root 执行。

## 1. 构建发布包

需要 Go 1.24+，在自己的电脑上：

```sh
make test     # go vet + 单元测试
make dist     # dist/vps-probe-<版本>-linux-{amd64,arm64}.tar.gz 及 .sha256
```

版本号在 `VERSION` 文件里。每个发布包都包含两个程序、`install.sh`、systemd 服务文件和示例配置；同一个包既能装服务端也能装 agent。VPS 是 ARM 的就用 arm64 包（`uname -m` 显示 `aarch64`）。

## 2. 安装服务端

把发布包传到服务端 VPS 并解压：

```sh
scp dist/vps-probe-0.1.0-linux-amd64.tar.gz root@服务端IP:
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

## 3. 通过 Cloudflare Tunnel + Access 访问网页

网页没有自己的登录，靠 Cloudflare Access 挡在前面；配置 `cf_access` 后服务端还会自己校验 Access 签发的令牌，隧道之外的请求一律 403。

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

## 4. 添加节点（每台 VPS）

在**服务端**上：

```sh
vps-probe-server gen-token                 # 新节点的 token
vi /etc/vps-probe/server.yml               # nodes 里加一项：id、name、token、addr
vps-probe-server check -config /etc/vps-probe/server.yml
systemctl restart vps-probe-server
vps-probe-server agent-config -config /etc/vps-probe/server.yml \
    -node hk-1 -server 服务端IP:9527 -o /root/hk-1.yml
```

`agent-config` 生成该节点的 `agent.yml`（权限 0600）：token、服务端地址，以及所有写了 `addr` 的其他节点作为 ping 对象。它只是生成一个文件，agent 不会从服务端拉取任何东西。

在**这台 VPS** 上：

```sh
scp root@服务端IP:/root/hk-1.yml .          # 或者用你自己的方式把文件拷过来
tar xzf vps-probe-0.1.0-linux-amd64.tar.gz && cd vps-probe-0.1.0-linux-amd64
./install.sh agent --config ../hk-1.yml
rm ../hk-1.yml                               # 服务端上的 /root/hk-1.yml 也删掉
```

看到 `agent is reporting: the server acknowledged it` 就成功了。

- 如果系统的 `net.ipv4.ping_group_range` 不允许普通用户 ping，`install.sh` 会只给 agent 服务加 `CAP_NET_RAW`（`/etc/systemd/system/vps-probe-agent.service.d/icmp.conf`），不改系统设置。
- 默认自动识别物理网卡（有 `/sys/class/net/<网卡>/device` 的）。识别不到时在 agent.yml 里写 `interfaces: [eth0]`。
- 新增节点后，想让已有节点也 ping 它：给这些节点重新生成 agent.yml，再各自 `./install.sh agent --config ...`。
- 服务端那台机器本身也可以装 agent，`-server` 写 `127.0.0.1:9527` 即可。

## 5. 告警与 Telegram

- 规则写在 `server.yml` 的 `alerts` 里（示例见 `examples/server.example.yml`，完整说明见 `docs/DESIGN.md` §7）。不写则用默认规则：离线 60s、CPU/内存 > 90% 持续 5 分钟、磁盘 > 90% 持续 10 分钟、链路丢包 > 20% 持续 3 分钟、流量配额 80/90/100%。
- Telegram：在 @BotFather 用 `/newbot` 建一个**专用** bot，给它发一条消息，再打开 `https://api.telegram.org/bot<TOKEN>/getUpdates` 找 `"chat":{"id":` 后面的数字。填进 `telegram.bot_token` / `telegram.chat_id`，然后：

  ```sh
  runuser -u vps-probe-server -- vps-probe-server test-telegram -config /etc/vps-probe/server.yml
  systemctl restart vps-probe-server
  ```

- 未配置 Telegram 时告警照常评估，记录在网页「告警」页，消息内容写入 `journalctl -u vps-probe-server`。

## 6. 升级

用新的发布包，在各台机器上重新运行同样的命令即可，**不带 `--config` 时保留现有配置**：

```sh
./install.sh server      # 服务端
./install.sh agent       # 每台 VPS
```

agent 停止前会把流量最后读一次并保存，升级不会丢月流量；服务端数据库结构有变化时启动时自动升级（只加表，不删数据）。建议升级服务端前先做一次备份（见下节）。

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
./install.sh uninstall agent --purge    # 连同配置、流量状态、系统用户一起删除
./install.sh uninstall server --purge   # 连同配置、数据库、备份、系统用户一起删除
```

服务端和 agent 装在同一台机器上时，卸载其中一个不会影响另一个。

## 9. 排查

| 现象 | 看哪里 |
|---|---|
| agent 日志一直 `no acknowledgement from server` | 服务端 `journalctl -u vps-probe-server`：`authentication failed` = token 不一致；`packet for a node not in the config` = node id 没登记；`timestamp too far` = 时钟偏差（检查 NTP）。都没有 = 包没到服务端，查安全组 / 防火墙的 UDP 9527 |
| 丢弃计数 | 网页底部；或未开 `cf_access` 时在服务端 `curl -s 127.0.0.1:8080/api/stats` |
| 网页 403 | 开了 `cf_access`：只能经 Cloudflare Access 访问；服务端日志 `cf_access: request rejected` 会写明原因 |
| 时延矩阵里对不上 | agent 的 `ping.peers[].name` 要写对端的节点 id；用 `agent-config` 生成的配置自动满足 |
| agent 起不来：`no physical network interface detected` | 在 agent.yml 写 `interfaces: [网卡名]` |
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
