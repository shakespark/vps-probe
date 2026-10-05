package config

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/shakespark/vps-probe/internal/agent/traffic"
	"github.com/shakespark/vps-probe/internal/peer"
)

const minimal = `
node: hk-1
server: 203.0.113.1:9527
token: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Disks) != 1 || c.Disks[0] != "/" || len(c.Interfaces) != 0 || len(c.Peers) != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Traffic.Reset != (traffic.Reset{Day: 1}) || c.Traffic.Location.String() != "Asia/Shanghai" {
		t.Fatalf("traffic: %+v", c.Traffic)
	}
}

func TestFull(t *testing.T) {
	c, err := Parse([]byte(minimal + `
disks: ["/", "/data"]
interfaces: [eth0]
traffic:
  timezone: UTC
  reset_day: 15
  reset_time: "18:21"
peers:
  - { name: jp-1, addr: 203.0.113.5 }
  - { name: us-1, addr: us1.example.com }
  - { name: cf-relay, addr: "127.0.0.1:15353", type: dns }
  - { name: hk2-tun, addr: "127.0.0.1:39527", type: echo, key: abcdefghijklmnopqrstuvwxyz0123456789 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Traffic.Reset != (traffic.Reset{Day: 15, Hour: 18, Minute: 21}) || c.Traffic.Location.String() != "UTC" {
		t.Fatalf("traffic: %+v", c.Traffic)
	}
	if len(c.Peers) != 4 || c.Peers[0].Type != peer.ICMP || c.Peers[2].Type != peer.DNS || c.Peers[3].Key == "" || c.Interfaces[0] != "eth0" {
		t.Fatalf("got %+v", c)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":    minimal + "interval: 5s\n",
		"bad node":       strings.Replace(minimal, "hk-1", "hk 1", 1),
		"short token":    strings.Replace(minimal, "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", "short", 1),
		"no port":        strings.Replace(minimal, "203.0.113.1:9527", "203.0.113.1", 1),
		"reset day":      minimal + "traffic:\n  reset_day: 32\n",
		"reset time":     minimal + "traffic:\n  reset_time: \"24:00\"\n",
		"reset time fmt": minimal + "traffic:\n  reset_time: 18h21m\n",
		"timezone":       minimal + "traffic:\n  timezone: Mars/Olympus\n",
		"relative disk":  minimal + "disks: [data]\n",
		"dup peer":       minimal + "peers:\n  - {name: a, addr: 1.1.1.1}\n  - {name: a, addr: 8.8.8.8}\n",
		"peer no addr":   minimal + "peers:\n  - {name: a}\n",
		"peer type":      minimal + "peers:\n  - {name: a, addr: 1.1.1.1, type: tcp}\n",
		"dns no port":    minimal + "peers:\n  - {name: a, addr: 1.1.1.1, type: dns}\n",
		"dns port 0":     minimal + "peers:\n  - {name: a, addr: \"1.1.1.1:0\", type: dns}\n",
		"echo no key":    minimal + "peers:\n  - {name: a, addr: \"1.1.1.1:39527\", type: echo}\n",
		"echo short key": minimal + "peers:\n  - {name: a, addr: \"1.1.1.1:39527\", type: echo, key: short}\n",
		"key on icmp":    minimal + "peers:\n  - {name: a, addr: 1.1.1.1, key: abcdefghijklmnopqrstuvwxyz0123456789}\n",
	}
	for name, cfg := range cases {
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The example config that ships in the release loads, and its commented-out
// lines do too once uncommented.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/agent.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err == nil {
		t.Fatal("the placeholder token was accepted")
	}
	data = bytes.Replace(data, []byte("<这个节点的 token>"), []byte("abcdefghijklmnopqrstuvwxyz0123456789"), 1)
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Node != "hk-1" || len(c.Peers) != 2 || c.Traffic.Reset != (traffic.Reset{Day: 1}) {
		t.Fatalf("example: %+v", c)
	}
	var all []string
	for _, l := range strings.Split(string(data), "\n") {
		// Settings are commented out with "# " at the line's indentation;
		// prose comments are Chinese sentences, which no setting starts with.
		trimmed := strings.TrimLeft(l, " ")
		if rest, ok := strings.CutPrefix(trimmed, "# "); ok && (strings.HasPrefix(rest, "- {") || strings.HasPrefix(rest, "disks:") || strings.HasPrefix(rest, "interfaces:")) {
			l = l[:len(l)-len(trimmed)] + rest
		}
		all = append(all, strings.Replace(l, "<那边的 key>", "abcdefghijklmnopqrstuvwxyz0123456789", 1))
	}
	c, err = Parse([]byte(strings.Join(all, "\n")))
	if err != nil {
		t.Fatalf("with the commented-out settings: %v", err)
	}
	if len(c.Peers) != 5 || len(c.Disks) != 2 || len(c.Interfaces) != 1 {
		t.Fatalf("with the commented-out settings: %+v", c)
	}
}
