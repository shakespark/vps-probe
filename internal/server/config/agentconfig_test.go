package config

import (
	"slices"
	"strings"
	"testing"

	agentconfig "github.com/shakespark/vps-probe/internal/agent/config"
	"github.com/shakespark/vps-probe/internal/agent/traffic"
	"github.com/shakespark/vps-probe/internal/peer"
)

func TestAgentConfigRoundTrip(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - {id: hk-1, token: ` + tokA + `, ping: {addr: 203.0.113.5}, traffic: {reset_day: 15, reset_time: "8:05"}}
  - {id: jp-1, token: ` + tokB + `, ping: {addr: jp.example.com}}
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
	if a.Node != "hk-1" || a.Token != tokA || a.Server != "198.51.100.1:9527" ||
		a.Traffic.Reset != (traffic.Reset{Day: 15, Hour: 8, Minute: 5}) || a.Traffic.Timezone != c.Timezone {
		t.Fatalf("got %+v", a)
	}
	if len(a.Peers) != 1 || a.Peers[0].Name != "jp-1" || a.Peers[0].Addr != "jp.example.com" {
		t.Fatalf("peers %+v", a.Peers)
	}
	if !strings.Contains(out, "没有 ping（server.yml 里没写 ping.addr）：us-1") {
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
		if _, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", ping: {addr: '" + addr + "'}}\n")); err == nil {
			t.Errorf("addr %q accepted", addr)
		}
	}
}

func TestNoPing(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - {id: a, token: ` + tokA + `, ping: {addr: a.example.com, exclude: [c]}}
  - {id: b, token: ` + tokB + `, ping: {addr: b.example.com}}
  - {id: c, token: cdefghijklmnopqrstuvwxyz0123456789ab, ping: {addr: c.example.com}}
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
		for _, p := range a.Peers {
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
		if id != "b" && !strings.Contains(out, "没有 ping（server.yml 里的 ping.exclude）：") {
			t.Errorf("%s: no note about the excluded node:\n%s", id, out)
		}
	}

	for _, np := range []string{"[x]", "[a]", "[b, b]"} {
		if _, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", ping: {exclude: " + np + "}}\n  - {id: b, token: " + tokB + "}\n")); err == nil {
			t.Errorf("ping.exclude %s accepted", np)
		}
	}
}

func TestExtraPeers(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - id: a
    token: ` + tokA + `
    ping:
      extra:
        - {name: cf-ppp, addr: 1.1.1.1}
        - {name: cf-relay, addr: "127.0.0.1:15353", type: dns}
        - {name: tun, addr: "127.0.0.1:39527", type: echo, key: ` + tokB + `}
  - {id: b, token: ` + tokB + `, ping: {addr: b.example.com}}
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
	want := []peer.Peer{
		{Name: "b", Addr: "b.example.com", Type: peer.ICMP},
		{Name: "cf-ppp", Addr: "1.1.1.1", Type: peer.ICMP},
		{Name: "cf-relay", Addr: "127.0.0.1:15353", Type: peer.DNS},
		{Name: "tun", Addr: "127.0.0.1:39527", Type: peer.Echo, Key: tokB},
	}
	if !slices.Equal(a.Peers, want) {
		t.Fatalf("peers %+v, want %+v", a.Peers, want)
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
		cfg := "nodes:\n  - {id: a, token: " + tokA + ", ping: {extra: [" + p + "]}}\n  - {id: b, token: " + tokB + "}\n"
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := "nodes:\n  - {id: a, token: " + tokA + ", ping: {extra: [{name: x, addr: 1.1.1.1}, {name: x, addr: 8.8.8.8}]}}\n"
	if _, err := Parse([]byte(dup)); err == nil {
		t.Error("duplicate accepted")
	}
}
