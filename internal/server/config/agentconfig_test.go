package config

import (
	"strings"
	"testing"

	agentconfig "vpsprobe/internal/agent/config"
)

func TestAgentConfigRoundTrip(t *testing.T) {
	c, err := Parse([]byte(`
nodes:
  - {id: hk-1, token: ` + tokA + `, addr: 203.0.113.5, reset_day: 15}
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
	if a.Node != "hk-1" || a.Server.Token != tokA || a.Server.Addr != "198.51.100.1:9527" || a.Traffic.ResetDay != 15 {
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
