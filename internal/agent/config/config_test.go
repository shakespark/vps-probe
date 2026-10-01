package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
node: hk-1
server:
  addr: 203.0.113.1:9527
  token: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 10*time.Second || c.StateDir != "/var/lib/vps-probe" ||
		len(c.Disks) != 1 || c.Disks[0] != "/" || len(c.Interfaces) != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Traffic.ResetDay != 1 || c.Traffic.Location.String() != "Asia/Shanghai" {
		t.Fatalf("traffic: %+v", c.Traffic)
	}
	if c.Ping.Interval != time.Second || c.Ping.Timeout != 2*time.Second {
		t.Fatalf("ping: %+v", c.Ping)
	}
}

func TestFull(t *testing.T) {
	c, err := Parse([]byte(minimal + `
interval: 5s
state_dir: /tmp/probe
disks: ["/", "/data"]
interfaces: [eth0]
traffic:
  timezone: UTC
  reset_day: 15
  reset_time: "18:21"
ping:
  interval: 500ms
  timeout: 1s
  peers:
    - { name: jp-1, addr: 203.0.113.5 }
    - { name: us-1, addr: us1.example.com }
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 5*time.Second || c.Traffic.ResetDay != 15 || c.Traffic.ResetHour != 18 || c.Traffic.ResetMinute != 21 || len(c.Ping.Peers) != 2 || c.Interfaces[0] != "eth0" {
		t.Fatalf("got %+v", c)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":     minimal + "intervall: 5s\n",
		"bad node":        strings.Replace(minimal, "hk-1", "hk 1", 1),
		"short token":     strings.Replace(minimal, "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", "short", 1),
		"no port":         strings.Replace(minimal, "203.0.113.1:9527", "203.0.113.1", 1),
		"reset day":       minimal + "traffic:\n  reset_day: 32\n",
		"reset time":      minimal + "traffic:\n  reset_time: \"24:00\"\n",
		"reset time fmt":  minimal + "traffic:\n  reset_time: 18h21m\n",
		"timezone":        minimal + "traffic:\n  timezone: Mars/Olympus\n",
		"relative disk":   minimal + "disks: [data]\n",
		"timeout too big": minimal + "ping:\n  timeout: 30s\n",
		"dup peer":        minimal + "ping:\n  peers:\n    - {name: a, addr: 1.1.1.1}\n    - {name: a, addr: 8.8.8.8}\n",
		"peer no addr":    minimal + "ping:\n  peers:\n    - {name: a}\n",
		"interval":        minimal + "interval: 100ms\n",
	}
	for name, cfg := range cases {
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
