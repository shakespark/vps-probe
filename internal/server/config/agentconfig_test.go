package config

import (
	"strings"
	"testing"

	agentconfig "vpsprobe/internal/agent/config"
)

func TestAgentConfigRoundTrip(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - {id: hk-1, token: ` + tokA + `, addr: 203.0.113.5, reset_day: 15, reset_time: "8:05"}
  - {id: jp-1, token: ` + tokB + `, addr: jp.example.com}
  - {id: us-1, token: cdefghijklmnopqrstuvwxyz0123456789ab}
`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.AgentConfig("hk-1", "198.51.100.1:9527")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agentconfig.Parse([]byte(out))
	if err != nil {
		t.Fatalf("generated config does not load: %v\n%s", err, out)
	}
	if a.Node != "hk-1" || a.Server.Token != tokA || a.Server.Addr != "198.51.100.1:9527" || a.Traffic.ResetDay != 15 || a.Traffic.ResetHour != 8 || a.Traffic.ResetMinute != 5 {
		t.Fatalf("got %+v", a)
	}
	if len(a.Ping.Peers) != 1 || a.Ping.Peers[0].Name != "jp-1" || a.Ping.Peers[0].Addr != "jp.example.com" {
		t.Fatalf("peers %+v", a.Ping.Peers)
	}
	if !strings.Contains(out, "Not pinged (no addr in server.yml): us-1") {
		t.Fatalf("missing-addr note:\n%s", out)
	}
	// A node with no peers still produces a valid file.
	out, _ = c.AgentConfig("us-1", "[2001:db8::1]:9527")
	if _, err := agentconfig.Parse([]byte(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := c.AgentConfig("nope", "1.2.3.4:9527"); err == nil {
		t.Fatal("unknown node accepted")
	}
	if _, err := c.AgentConfig("hk-1", "1.2.3.4"); err == nil {
		t.Fatal("server address without port accepted")
	}
}

func TestBadAddr(t *testing.T) {
	for _, addr := range []string{"1.2.3.4:9527", "a b", "-", "x..y", "http://a"} {
		if _, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", addr: '" + addr + "'}\n")); err == nil {
			t.Errorf("addr %q accepted", addr)
		}
	}
}

func TestNoPing(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - {id: a, token: ` + tokA + `, addr: a.example.com, no_ping: [c]}
  - {id: b, token: ` + tokB + `, addr: b.example.com}
  - {id: c, token: cdefghijklmnopqrstuvwxyz0123456789ab, addr: c.example.com}
`))
	if err != nil {
		t.Fatal(err)
	}
	peers := func(id string) (names []string, out string) {
		out, err := c.AgentConfig(id, "198.51.100.1:9527")
		if err != nil {
			t.Fatal(err)
		}
		a, err := agentconfig.Parse([]byte(out))
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, p := range a.Ping.Peers {
			names = append(names, p.Name)
		}
		return names, out
	}
	// Both directions are dropped, other pairs are kept.
	for id, want := range map[string]string{"a": "b", "b": "a,c", "c": "b"} {
		got, out := peers(id)
		if strings.Join(got, ",") != want {
			t.Errorf("%s pings %v, want %s", id, got, want)
		}
		if id != "b" && !strings.Contains(out, "Not pinged (no_ping in server.yml): ") {
			t.Errorf("%s: no no_ping note:\n%s", id, out)
		}
	}

	for _, np := range []string{"[x]", "[a]", "[b, b]"} {
		if _, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", no_ping: " + np + "}\n  - {id: b, token: " + tokB + "}\n")); err == nil {
			t.Errorf("no_ping %s accepted", np)
		}
	}
}

func TestExtraPeers(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - id: a
    token: ` + tokA + `
    extra_peers:
      - {name: cf-ppp, addr: 1.1.1.1}
      - {name: cf-relay, addr: "127.0.0.1:15353", type: dns}
      - {name: tun, addr: "127.0.0.1:39527", type: echo, key: ` + tokB + `}
  - {id: b, token: ` + tokB + `, addr: b.example.com}
`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.AgentConfig("a", "198.51.100.1:9527")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agentconfig.Parse([]byte(out))
	if err != nil {
		t.Fatalf("generated config does not load: %v\n%s", err, out)
	}
	want := []agentconfig.Peer{
		{Name: "b", Addr: "b.example.com"},
		{Name: "cf-ppp", Addr: "1.1.1.1"},
		{Name: "cf-relay", Addr: "127.0.0.1:15353", Type: "dns"},
		{Name: "tun", Addr: "127.0.0.1:39527", Type: "echo", Key: tokB},
	}
	if len(a.Ping.Peers) != len(want) {
		t.Fatalf("peers %+v", a.Ping.Peers)
	}
	for i := range want {
		if a.Ping.Peers[i] != want[i] {
			t.Fatalf("peer %d = %+v, want %+v", i, a.Ping.Peers[i], want[i])
		}
	}
	if strings.Contains(out, `"icmp"`) {
		t.Fatalf("type written for an icmp peer (older agents reject it):\n%s", out)
	}
	// Extra peers are only this node's.
	out, _ = c.AgentConfig("b", "198.51.100.1:9527")
	if strings.Contains(out, "cf-") {
		t.Fatalf("b got a's extra peers:\n%s", out)
	}
}

func TestBadExtraPeers(t *testing.T) {
	for name, p := range map[string]string{
		"node id":     "{name: b, addr: 1.1.1.1}",
		"bad name":    "{name: 'a b', addr: 1.1.1.1}",
		"icmp port":   "{name: x, addr: '1.1.1.1:53'}",
		"dns no port": "{name: x, addr: 1.1.1.1, type: dns}",
		"dns port 0":  "{name: x, addr: '1.1.1.1:0', type: dns}",
		"type":        "{name: x, addr: 1.1.1.1, type: tcp}",
		"echo no key": "{name: x, addr: '1.1.1.1:39527', type: echo}",
		"key on dns":  "{name: x, addr: '1.1.1.1:53', type: dns, key: " + tokB + "}",
	} {
		cfg := "nodes:\n  - {id: a, token: " + tokA + ", extra_peers: [" + p + "]}\n  - {id: b, token: " + tokB + "}\n"
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := "nodes:\n  - {id: a, token: " + tokA + ", extra_peers: [{name: x, addr: 1.1.1.1}, {name: x, addr: 8.8.8.8}]}\n"
	if _, err := Parse([]byte(dup)); err == nil {
		t.Error("duplicate accepted")
	}
}
