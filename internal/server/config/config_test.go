package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tokA = "abcdefghijklmnopqrstuvwxyz0123456789"
const tokB = "bcdefghijklmnopqrstuvwxyz0123456789a"

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("nodes:\n  - {id: hk-1, token: " + tokA + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Web != "127.0.0.1:8080" || c.Listen.Ingest != ":9527" || c.Location.String() != "Asia/Shanghai" {
		t.Fatalf("defaults: %+v", c)
	}
	n := c.Nodes[0]
	if n.Name != "hk-1" || n.QuotaMode != "sum" {
		t.Fatalf("node defaults: %+v", n)
	}
	c, err = Parse([]byte("nodes:\n  - {id: hk-1, token: " + tokA + ", region: us-la, group: 美国}\n"))
	if err != nil || c.Nodes[0].Region != "US-LA" || c.Nodes[0].Group != "美国" {
		t.Fatalf("region/group: %+v %v", c.Nodes[0], err)
	}
	if time.Duration(c.Retention.H1) != 400*24*time.Hour || c.Backup.Keep != 7 {
		t.Fatalf("retention/backup: %+v %+v", c.Retention, c.Backup)
	}
}

func TestDurations(t *testing.T) {
	c, err := Parse([]byte("retention: {raw: 3d, m5: 60d, h1: 9600h}\nnodes:\n  - {id: a, token: " + tokA + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.Retention.Raw) != 72*time.Hour || time.Duration(c.Retention.H1) != 9600*time.Hour {
		t.Fatalf("%+v", c.Retention)
	}
}

func TestRejects(t *testing.T) {
	for name, yml := range map[string]string{
		"no nodes":        "db: /x.db\n",
		"unknown key":     "nodes:\n  - {id: a, token: " + tokA + "}\ntypo: 1\n",
		"short token":     "nodes:\n  - {id: a, token: short}\n",
		"shared token":    "nodes:\n  - {id: a, token: " + tokA + "}\n  - {id: b, token: " + tokA + "}\n",
		"duplicate id":    "nodes:\n  - {id: a, token: " + tokA + "}\n  - {id: a, token: " + tokB + "}\n",
		"bad id":          "nodes:\n  - {id: 'a b', token: " + tokA + "}\n",
		"bad quota mode":  "nodes:\n  - {id: a, token: " + tokA + ", traffic_quota_mode: both}\n",
		"relative db":     "db: probe.db\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad timezone":    "timezone: Mars/Base\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"raw too short":   "retention: {raw: 1h}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"retention order": "retention: {raw: 10d, m5: 5d}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad listen":      "listen: {web: 8080}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad expire_at":   "nodes:\n  - {id: a, token: " + tokA + ", expire_at: 2026-02-30}\n",
		"renew alone":     "nodes:\n  - {id: a, token: " + tokA + ", renew_months: 1}\n",
		"long price":      "nodes:\n  - {id: a, token: " + tokA + ", price: '" + strings.Repeat("x", MaxPriceLen+1) + "'}\n",
		"price newline":   "nodes:\n  - {id: a, token: " + tokA + ", price: \"a\\nb\"}\n",
		"bad region":      "nodes:\n  - {id: a, token: " + tokA + ", region: 香港}\n",
		"long region":     "nodes:\n  - {id: a, token: " + tokA + ", region: ABCDEFGHI}\n",
		"long group":      "nodes:\n  - {id: a, token: " + tokA + ", group: '" + strings.Repeat("组", MaxGroupLen+1) + "'}\n",
	} {
		if _, err := Parse([]byte(yml)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRefusesReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yml")
	os.WriteFile(path, []byte("nodes:\n  - {id: a, token: "+tokA+"}\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chmod 640") {
		t.Fatalf("0644 accepted: %v", err)
	}
	os.Chmod(path, 0o640)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestAlertRules(t *testing.T) {
	c, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + "}\n  - {id: b, token: " + tokB + "}\n" + `
telegram:
  bot_token: "123:abc"
  chat_id: -1001234567890
alerts:
  - {name: cpu, metric: cpu, op: ">", threshold: 90, for: 5m, repeat: 1h}
  - {name: disk, metric: disk, op: ">=", threshold: 85, nodes: [b], notify_recovery: false}
  - {name: down, metric: offline, for: 60s, nodes: all}
  - {name: quota, metric: traffic, levels: [50, 100]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.ChatID != "-1001234567890" || !c.Telegram.Enabled() {
		t.Fatalf("telegram: %+v", c.Telegram)
	}
	r := c.Alerts
	if len(r) != 4 || !r[0].Nodes.Has("a") || r[1].Nodes.Has("a") || !r[1].Nodes.Has("b") {
		t.Fatalf("nodes: %+v", r)
	}
	if !r[0].Recovers() || r[1].Recovers() || time.Duration(r[0].Repeat) != time.Hour {
		t.Fatalf("recovery/repeat: %+v", r)
	}
}

func TestDefaultAndDisabledAlerts(t *testing.T) {
	c, _ := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + "}\n"))
	if len(c.Alerts) != len(DefaultRules()) || c.Telegram.Enabled() {
		t.Fatalf("defaults: %d rules", len(c.Alerts))
	}
	c, err := Parse([]byte("alerts: []\nnodes:\n  - {id: a, token: " + tokA + "}\n"))
	if err != nil || len(c.Alerts) != 0 {
		t.Fatalf("alerts: [] -> %d rules, %v", len(c.Alerts), err)
	}
}

func TestBadAlertRules(t *testing.T) {
	head := "nodes:\n  - {id: a, token: " + tokA + "}\nalerts:\n"
	for name, rule := range map[string]string{
		"unknown metric":    "  - {name: x, metric: temp, op: '>', threshold: 1}",
		"missing threshold": "  - {name: x, metric: cpu, op: '>'}",
		"bad op":            "  - {name: x, metric: cpu, op: '=', threshold: 1}",
		"offline threshold": "  - {name: x, metric: offline, for: 1m, threshold: 3}",
		"offline no for":    "  - {name: x, metric: offline}",
		"traffic no levels": "  - {name: x, metric: traffic}",
		"traffic unsorted":  "  - {name: x, metric: traffic, levels: [90, 80]}",
		"expiry no levels":  "  - {name: x, metric: expiry}",
		"expiry threshold":  "  - {name: x, metric: expiry, levels: [7], op: '<', threshold: 7}",
		"expiry fraction":   "  - {name: x, metric: expiry, levels: [1.5]}",
		"expiry duplicate":  "  - {name: x, metric: expiry, levels: [7, 1, 7]}",
		"expiry negative":   "  - {name: x, metric: expiry, levels: [-1]}",
		"ip_change for":     "  - {name: x, metric: ip_change, for: 1m}",
		"ip_change levels":  "  - {name: x, metric: ip_change, levels: [1]}",
		"unknown node":      "  - {name: x, metric: cpu, op: '>', threshold: 1, nodes: [zz]}",
		"nodes typo":        "  - {name: x, metric: cpu, op: '>', threshold: 1, nodes: everyone}",
		"short repeat":      "  - {name: x, metric: cpu, op: '>', threshold: 1, repeat: 10s}",
		"duplicate name":    "  - {name: x, metric: offline, for: 1m}\n  - {name: x, metric: offline, for: 2m}",
		"half telegram":     "  - {name: x, metric: offline, for: 1m}\ntelegram: {bot_token: '1:a'}",
	} {
		if _, err := Parse([]byte(head + rule + "\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCFAccess(t *testing.T) {
	base := "nodes:\n  - {id: a, token: " + tokA + "}\n"
	aud := "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2"
	c, err := Parse([]byte(base + "cf_access: {team_domain: myteam.cloudflareaccess.com, aud: " + aud + "}\n"))
	if err != nil || !c.CFAccess.Enabled() {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"cf_access: {team_domain: myteam.example.com, aud: " + aud + "}",
		"cf_access: {team_domain: myteam.cloudflareaccess.com, aud: abc}",
		"cf_access: {team_domain: myteam.cloudflareaccess.com}",
		"cf_access: {aud: " + aud + "}",
	} {
		if _, err := Parse([]byte(base + bad + "\n")); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestExpiry(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	at := func(s string) time.Time { // a UTC instant
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		expire string
		renew  int
		now    time.Time
		date   string
		days   int
	}{
		{"2026-10-10", 0, at("2026-09-30T12:00:00Z"), "2026-10-10", 10},
		{"2026-10-10", 0, at("2026-10-10T15:59:00Z"), "2026-10-10", 0}, // 23:59 in Shanghai
		{"2026-10-10", 0, at("2026-10-10T16:00:00Z"), "2026-10-10", -1},
		{"2026-01-31", 1, at("2026-02-15T00:00:00Z"), "2026-02-28", 13}, // clamped
		{"2026-01-31", 1, at("2026-03-01T00:00:00Z"), "2026-03-31", 30}, // from the anchor, not from Feb 28
		{"2026-01-31", 1, at("2026-01-30T18:00:00Z"), "2026-01-31", 0},  // the day itself is not past
		{"2020-02-29", 12, at("2026-03-01T00:00:00Z"), "2027-02-28", 364},
		{"2024-03-15", 3, at("2026-09-30T00:00:00Z"), "2026-12-15", 76},
	} {
		n := Node{ExpireAt: c.expire, RenewMonths: c.renew}
		n.expire, _ = time.Parse(time.DateOnly, c.expire)
		date, days, ok := n.Expiry(c.now, sh)
		if !ok || date != c.date || days != c.days {
			t.Errorf("%s every %d at %s: %s, %d days; want %s, %d", c.expire, c.renew, c.now, date, days, c.date, c.days)
		}
	}
	if _, _, ok := (&Node{}).Expiry(time.Now(), sh); ok {
		t.Error("no expire_at: ok")
	}
	c, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", expire_at: 2027-03-15, renew_months: 12, price: $10/年}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if date, _, ok := c.Nodes[0].Expiry(at("2026-10-01T00:00:00Z"), c.Location); !ok || date != "2027-03-15" || c.Nodes[0].Price != "$10/年" {
		t.Fatalf("parsed: %s %+v", date, c.Nodes[0])
	}
}
