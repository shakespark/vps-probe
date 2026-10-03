# 告警、报告与通知

值得发一条消息的事有两种，在 `server.yml` 里分开写：

- **告警规则**（`alerts`）：某个指标满足条件并持续一段时间后告警，不再满足后发恢复。
- **报告**（`reports`）：发生一次就通知一次的事，没有"恢复"：流量到了配额的某一档、快到期了、周期结算、每周汇总。

两段都可以整段不写，用默认的一组；写成空列表（`alerts: []`）表示不要。**写了就只用写的那些**，所以想在默认的基础上改，先把示例配置里那一段抄过来。改完 `vps-probe-server check`，再重启服务端。

每 10 秒评估一轮，同一轮里的消息合并成一条发出。网页的「告警」页能看到当前告警、历史和生效的规则。

## 告警规则

```yaml
alerts:
  - name: cpu_high          # 规则名，出现在消息和历史里
    metric: cpu
    op: ">"                 # > | >= | < | <=
    threshold: 90
    for: 5m                 # 持续多久才告警；不写 = 一满足就告警
    # nodes: [hk-1, jp-1]   # 只对这些节点；默认全部
    # exclude: [home-1]     # 全部节点中排除这些（以后新加的节点自动包含）
    # repeat: 1h            # 仍未恢复时每隔多久提醒一次；默认不提醒
    # notify_recovery: false   # 恢复时不发消息；默认发
```

- 告警之后，条件要连续 `for`（最多 1 分钟）不满足才算恢复，免得在阈值附近来回跳。
- 节点离线时，它身上其他规则的告警保持不动，既不恢复也不重复：它最后的数值是旧的。
- 服务端重启不会重复告警，也不会忘记正在告警的。

### 指标

| `metric` | 含义 | 单位 |
|---|---|---|
| `offline` | 超过 `for` 没有收到报文。不写 `op` 和 `threshold`；`for` 至少 30 秒 | — |
| `cpu` | CPU 使用率 | % |
| `steal` | 被宿主机拿走的 CPU 时间，高说明商家超售 | % |
| `softirq` | 软中断占 CPU 的比例，主要是处理网络包的开销 | % |
| `load1` | 1 分钟负载 | — |
| `mem`、`swap` | 内存、Swap 使用率 | % |
| `disk` | 磁盘使用率，每个挂载点分别判断 | % |
| `ping_loss` | 丢包率，每条链路（发起 → 目标）分别判断，按最近 60 秒汇总 | % |
| `ping_avg` | 平均时延，同上 | ms |
| `net_in`、`net_out` | 入站、出站速率，最近 60 秒平均，各网卡相加 | Mbps |
| `pps_in`、`pps_out` | 入站、出站包速率，同上 | 包/秒 |

- 页面上节点 30 秒没有报文就显示离线；默认的 `offline` 规则是 60 秒才发消息。
- 链路的目标节点离线、并且有 `offline` 规则管着它时，指向它的链路不报丢包：节点宕机时所有对端必然全丢，离线告警已经说明了。目标还在上报、只是 ping 不通时照常告警。

### DDoS 识别

`net_in` / `net_out` 可以加 `ratio`：本方向还要不低于反方向的若干倍才算满足。

```yaml
  - name: ddos
    metric: net_in
    op: ">="
    threshold: 50           # Mbps
    ratio: 4                # 并且入站 ≥ 4 倍出站
    for: 2m
  - name: abuse_out         # 出站远大于入站：机器可能被利用去攻击别人
    metric: net_out
    op: ">="
    threshold: 50
    ratio: 4
    for: 5m
```

中转机的正常流量收发大致对称，被流量型攻击时入站远大于出站。有了比例条件，阈值不用按带宽精调，对称的大流量也不会误报。以下载为主的节点（入站多）或做种的节点（出站多）用 `exclude` 排除。

带 `ratio` 的规则，消息里会写"疑似 DDoS"和倍数，并附上佐证：

- 多少个节点到它丢包 ≥ 20%（入口被打满时所有对端同时丢包）；
- 包速率、入站平均包长、软中断；
- 被打后遭商家黑洞（节点离线）时，离线告警会附上停止上报前的入站峰值；
- 只封入站的黑洞不会让节点离线：入站回落后规则恢复，但如果多数对端仍然丢包，恢复消息会说明。

**包多流量小的攻击**（SYN flood、小包 UDP flood）字节速率看不出来，用 `pps_in` 和 `softirq`。它们不在默认规则里：阈值因机器而异，先看几天节点详情页的「包速率」图再定。

```yaml
  - name: pps_flood
    metric: pps_in
    op: ">="
    threshold: 50000
    for: 1m
```

## 报告

每种最多写一条，都可以带 `nodes` / `exclude`。

```yaml
reports:
  - type: traffic_quota     # 流量用到配额的这些百分比时提醒；每个周期每档一次
    levels: [80, 90, 100]
  - type: expiry            # 到期前这些天提醒（0 = 当天）；每个到期日每档一次
    days: [7, 1]
  - type: period            # 每个节点的流量周期结束时发一份结算
  - type: weekly            # 每周一份全部节点的流量汇总
    at: "Mon 09:00"         # 星期（Mon…Sun）和时刻，按 timezone
  # - type: ip_change       # 上报来源 IP 变了就通知；不在默认里
  #   nodes: [hk-1]
```

- `traffic_quota` 只管写了 `traffic.quota_gb` 的节点，按它的 `quota_mode` 算已用量。一次跨过几档只发最高的一档。
- `expiry` 只管写了 `plan.expire_at` 的节点。写了 `renew_months` 的，过了到期日自动顺延到下一个周期，重新计档。
- `period`：上下行、合计、配额使用率、日均、用量最多的一天。
- `weekly`：每个节点一行：本周期已用、近 7 天用量、按近 7 天的速率推算的周期末用量（可能超额时标 ⚠️）、重置时间；末尾列出 30 天内到期的节点。服务端在发送时刻停机的，24 小时内补发。
- `ip_change`：第一次见到的 IP 只记录不通知。动态 IP 的节点会经常触发，用 `nodes` 只选固定 IP 的。

## 通知渠道

`notify` 是一个列表，可以有多个渠道，每条消息每个渠道各发一次。**只发不收**：服务端不读取任何渠道发来的消息。一个都不配时，告警照常评估并记录在「告警」页，消息写进 `journalctl -u vps-probe-server`。

配好以后：

```sh
vps-probe-server test-notify          # 向每个渠道发一条测试消息，逐个报告结果
systemctl restart vps-probe-server
```

### Telegram

在 @BotFather 用 `/newbot` 建一个**专用**的 bot，给它发一条消息，再打开 `https://api.telegram.org/bot<TOKEN>/getUpdates`，找 `"chat":{"id":` 后面的数字。

```yaml
notify:
  - type: telegram
    bot_token: "123456789:AA..."
    chat_id: 123456789        # 群组是负数，不用加引号
```

要发到几个聊天就写几条，第二条起要起个名字（`name: ops-group`）。

### Webhook

其他推送服务都用 webhook：每条消息发一个 HTTP 请求。`{{title}}` 是消息第一行，`{{message}}` 是全文。写在 `url` 里会自动做 URL 编码；写在 JSON `body` 里会自动变成带引号的 JSON 字符串，所以**模板里不要再给它加引号**。

```yaml
notify:
  - type: webhook
    name: bark                         # 渠道名，出现在日志和告警页
    url: "https://api.day.app/你的KEY/{{title}}/{{message}}"
    method: GET
  - type: webhook
    name: ntfy
    url: "https://ntfy.sh/你的主题"
    headers: { Content-Type: text/plain }
    body: "{{message}}"
  - type: webhook
    name: discord                      # Slack 同理，把 content 换成 text
    url: "https://discord.com/api/webhooks/ID/TOKEN"
    body: '{"content": {{message}}}'
  - type: webhook
    name: serverchan                   # Server 酱
    url: "https://sctapi.ftqq.com/你的KEY.send"
    headers: { Content-Type: application/x-www-form-urlencoded }
    body: "title={{title}}&desp={{message}}"
  - type: webhook
    name: wecom                        # 企业微信群机器人
    url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=你的KEY"
    body: '{"msgtype": "text", "text": {"content": {{message}}}}'
```

- `method` 默认 POST；不写 `body` 时发 `{"title": ..., "message": ...}`；`Content-Type` 默认 `application/json`。
- 返回 2xx 算成功；429、5xx 和网络错误会重试；其他 4xx 不重试，记一条日志。
- 日志里只有渠道名和状态码，不会出现带密钥的 URL。
