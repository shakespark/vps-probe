package config

import (
	"fmt"
	"strings"

	"github.com/shakespark/vps-probe/internal/netaddr"
	"github.com/shakespark/vps-probe/internal/peer"
)

// AgentConfig renders agent.yml for one node: its token, its billing
// period, and its peers: every other node that has a ping.addr, except
// those excluded by either side, and its ping.extra. The result is a local
// file for the agent; nothing is ever pushed to agents.
func (c *Config) AgentConfig(id, serverAddr string) (string, error) {
	n, ok := c.Node(id)
	if !ok {
		return "", fmt.Errorf("node %q is not in the config", id)
	}
	if !netaddr.ValidHostPort(serverAddr) {
		return "", fmt.Errorf("server address %q: want host:port as the agent should reach it, e.g. 203.0.113.1:9527", serverAddr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 由 `vps-probe-server agent-config -node %s` 生成，安装：./install.sh agent --config <本文件>\n", id)
	fmt.Fprintf(&b, "# 含本节点的 token，不要公开。各项含义见发布包里的 examples/agent.example.yml。\n\n")
	fmt.Fprintf(&b, "node: %q\nserver: %q\ntoken: %q\n", n.ID, serverAddr, n.Token)
	fmt.Fprintf(&b, "traffic:\n  timezone: %q\n  reset_day: %d\n  reset_time: %q\n", c.Timezone, n.Traffic.ResetDay, n.Traffic.ResetTime)

	var peers []peer.Peer
	var noAddr, excluded []string
	for _, o := range c.Nodes {
		switch {
		case o.ID == id:
		case c.PingExcluded(id, o.ID):
			excluded = append(excluded, o.ID)
		case o.Ping.Addr == "":
			noAddr = append(noAddr, o.ID)
		default:
			peers = append(peers, peer.Peer{Name: o.ID, Addr: o.Ping.Addr, Type: peer.ICMP})
		}
	}
	peers = append(peers, n.Ping.Extra...)
	if len(peers) == 0 {
		b.WriteString("peers: []\n")
	} else {
		b.WriteString("peers:\n")
	}
	for _, p := range peers {
		fmt.Fprintf(&b, "  - { name: %q, addr: %q", p.Name, p.Addr)
		if p.Type != peer.ICMP {
			fmt.Fprintf(&b, ", type: %s", p.Type)
		}
		if p.Key != "" {
			fmt.Fprintf(&b, ", key: %q", p.Key)
		}
		b.WriteString(" }\n")
	}
	if len(noAddr) > 0 {
		fmt.Fprintf(&b, "# 没有 ping（server.yml 里没写 ping.addr）：%s\n", strings.Join(noAddr, ", "))
	}
	if len(excluded) > 0 {
		fmt.Fprintf(&b, "# 没有 ping（server.yml 里的 ping.exclude）：%s\n", strings.Join(excluded, ", "))
	}
	return b.String(), nil
}
