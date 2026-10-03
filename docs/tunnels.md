# 测经隧道的时延

节点之间的时延由 agent 互相 ping 得到。要测"经某条隧道出去"的时延，在发起探测的节点下加额外的目标（`server.yml` 里它的 `ping.extra`），再重新安装它的配置：

```sh
vps-probe-server install-cmd -node jp-1     # 把打印的命令在 jp-1 上运行
```

目标的 `name` 不能和节点 id 重名；它会作为单独的一列出现在时延矩阵里。

## VPN 型隧道（tun 设备）

直接 ping 隧道那头的地址，前提是到这个地址的路由走隧道：

```yaml
    ping:
      extra:
        - { name: cf-vpn, addr: 1.1.1.1 }
```

隧道进程退出、设备消失后，路由会回落到默认出口，测到的就变成直连的时延。

## 端口转发型隧道（realm、gost 等）

这类隧道只转发 TCP / UDP，ICMP 过不去；而 TCP 握手是转发程序在本地完成的，测 TCP 连接时延只反映到转发入口的一段。所以用 UDP 的一问一答来测，有两种办法。

### 隧道回显（推荐）

不经任何公共服务，测的就是隧道本身。

1. 在隧道的终点（比如 hk-1）装应答端。它是独立的程序和系统用户，只回应带正确签名的请求，其他包一律不回应：

   ```sh
   cp examples/echo.example.yml echo.yml
   vi echo.yml                               # key 用 vps-probe-server gen-token 生成
   ./install.sh echo --config echo.yml
   ```

   放行它的 UDP 端口（默认 39527）。可以用 `allow` 只回应中转机的出口 IP。
2. 在隧道上加一条转发规则，把入口机器的某个本地端口转到 `hk-1:39527`（UDP）。本地端口只监听 `127.0.0.1`。
3. 在发起探测的节点下加目标，`key` 和应答端的一样：

   ```yaml
       ping:
         extra:
           - { name: hk1-via-sgp, addr: "127.0.0.1:本地端口", type: echo, key: "<同一个 key>" }
   ```

两端的时钟相差不能超过 5 分钟。

### DNS

把隧道的远端指向一个公共 DNS，不用装应答端：

```yaml
    ping:
      extra:
        - { name: cf-relay, addr: "127.0.0.1:本地端口", type: dns }
```

转发规则把本地端口转到 `1.1.1.1:53`（UDP）。agent 每秒发一个 DNS 查询，以收到响应的时间为时延。

- 本地转发端口只监听 `127.0.0.1`；监听 `0.0.0.0` 的话它就成了公网开放的 DNS 中继。
- 测到的是"隧道 + 到 DNS 服务器"的时延，只宜和同样方式测的数据比较。
