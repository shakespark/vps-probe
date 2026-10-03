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
	if n.Name != "hk-1" || n.Traffic.QuotaMode != QuotaSum || n.Traffic.ResetDay != 1 || n.Traffic.ResetTime != "00:00" {
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
		"bad quota mode":  "nodes:\n  - {id: a, token: " + tokA + ", traffic: {quota_mode: both}}\n",
		"old node key":    "nodes:\n  - {id: a, token: " + tokA + ", traffic_quota_gb: 100}\n",
		"old top key":     "nodes:\n  - {id: a, token: " + tokA + "}\noffline_after: 30s\n",
		"relative db":     "db: probe.db\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad timezone":    "timezone: Mars/Base\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"raw too short":   "retention: {raw: 1h}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"retention order": "retention: {raw: 10d, m5: 5d}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad listen":      "listen: {web: 8080}\nnodes:\n  - {id: a, token: " + tokA + "}\n",
		"bad expire_at":   "nodes:\n  - {id: a, token: " + tokA + ", plan: {expire_at: 2026-02-30}}\n",
		"renew alone":     "nodes:\n  - {id: a, token: " + tokA + ", plan: {renew_months: 1}}\n",
		"long price":      "nodes:\n  - {id: a, token: " + tokA + ", plan: {price: '" + strings.Repeat("x", MaxPriceLen+1) + "'}}\n",
		"price newline":   "nodes:\n  - {id: a, token: " + tokA + ", plan: {price: \"a\\nb\"}}\n",
		"bad reset day":   "nodes:\n  - {id: a, token: " + tokA + ", traffic: {reset_day: 32}}\n",
		"bad reset time":  "nodes:\n  - {id: a, token: " + tokA + ", traffic: {reset_time: 25:00}}\n",
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
alerts:
  - {name: cpu, metric: cpu, op: ">", threshold: 90, for: 5m, repeat: 1h}
  - {name: disk, metric: disk, op: ">=", threshold: 85, nodes: [b], notify_recovery: false}
  - {name: down, metric: offline, for: 60s, nodes: all}
  - {name: idle, metric: load1, op: "<", threshold: 0}
  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4, for: 2m, exclude: [b]}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Alerts
	if len(r) != 5 || r[4].Ratio != 4 || r[4].Threshold != 50 || !r[4].Covers("a") || r[4].Covers("b") ||
		!r[1].Covers("b") || r[1].Covers("a") || r[3].Threshold != 0 {
		t.Fatalf("rules: %+v", r)
	}
	if !r[0].NotifyRecovery || r[1].NotifyRecovery || time.Duration(r[0].Repeat) != time.Hour {
		t.Fatalf("recovery/repeat: %+v", r)
	}
}

func TestDefaultAndDisabledAlerts(t *testing.T) {
	c, _ := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + "}\n"))
	if len(c.Alerts) != len(DefaultRules()) || len(c.Reports) != len(DefaultReports()) || len(c.Notify) != 0 {
		t.Fatalf("defaults: %d rules, %d reports", len(c.Alerts), len(c.Reports))
	}
	for _, r := range c.Alerts {
		if !r.NotifyRecovery {
			t.Errorf("default rule %s does not notify recovery", r.Name)
		}
	}
	c, err := Parse([]byte("alerts: []\nreports: []\nnodes:\n  - {id: a, token: " + tokA + "}\n"))
	if err != nil || len(c.Alerts) != 0 || len(c.Reports) != 0 {
		t.Fatalf("empty lists -> %d rules, %d reports, %v", len(c.Alerts), len(c.Reports), err)
	}
}

func TestBadAlertRules(t *testing.T) {
	head := "nodes:\n  - {id: a, token: " + tokA + "}\nalerts:\n"
	for name, rule := range map[string]string{
		"unknown metric":    "  - {name: x, metric: temp, op: '>', threshold: 1}",
		"a report's metric": "  - {name: x, metric: traffic, levels: [80]}",
		"a report's name":   "  - {name: weekly, metric: cpu, op: '>', threshold: 1}",
		"unknown key":       "  - {name: x, metric: cpu, op: '>', threshold: 1, treshold: 2}",
		"missing threshold": "  - {name: x, metric: cpu, op: '>'}",
		"bad op":            "  - {name: x, metric: cpu, op: '=', threshold: 1}",
		"offline threshold": "  - {name: x, metric: offline, for: 1m, threshold: 3}",
		"offline no for":    "  - {name: x, metric: offline}",
		"offline short for": "  - {name: x, metric: offline, for: 10s}",
		"exclude unknown":   "  - {name: x, metric: cpu, op: '>', threshold: 1, exclude: [zz]}",
		"exclude and nodes": "  - {name: x, metric: cpu, op: '>', threshold: 1, nodes: [a], exclude: [a]}",
		"exclude empty":     "  - {name: x, metric: cpu, op: '>', threshold: 1, exclude: []}",
		"ratio on softirq":  "  - {name: x, metric: softirq, op: '>', threshold: 1, ratio: 2}",
		"pps no threshold":  "  - {name: x, metric: pps_in, op: '>'}",
		"ratio on cpu":      "  - {name: x, metric: cpu, op: '>', threshold: 1, ratio: 2}",
		"ratio below 1":     "  - {name: x, metric: net_in, op: '>', threshold: 1, ratio: 0.5}",
		"ratio with <":      "  - {name: x, metric: net_in, op: '<', threshold: 1, ratio: 2}",
		"net no threshold":  "  - {name: x, metric: net_out, op: '>', ratio: 2}",
		"unknown node":      "  - {name: x, metric: cpu, op: '>', threshold: 1, nodes: [zz]}",
		"nodes typo":        "  - {name: x, metric: cpu, op: '>', threshold: 1, nodes: everyone}",
		"short repeat":      "  - {name: x, metric: cpu, op: '>', threshold: 1, repeat: 10s}",
		"duplicate name":    "  - {name: x, metric: offline, for: 1m}\n  - {name: x, metric: offline, for: 2m}",
	} {
		if _, err := Parse([]byte(head + rule + "\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReports(t *testing.T) {
	head := "nodes:\n  - {id: a, token: " + tokA + "}\n  - {id: b, token: " + tokB + "}\nreports:\n"
	c, err := Parse([]byte(head + `
  - {type: traffic_quota, levels: [50, 100]}
  - {type: expiry, days: [30, 7, 0], exclude: [b]}
  - {type: ip_change, nodes: [a]}
  - {type: period}
  - {type: weekly, at: "sun 8:05"}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Reports
	if len(r) != 5 || r[0].Levels[1] != 100 || r[1].Days[2] != 0 || r[1].Covers("b") || !r[2].Covers("a") || r[2].Covers("b") || r[4].At != "Sun 08:05" {
		t.Fatalf("reports: %+v", r)
	}
	sh := c.Location
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, sh) // a Wednesday
	if got := r[4].WeeklySlot(now, sh); !got.Equal(time.Date(2026, 10, 4, 8, 5, 0, 0, sh)) {
		t.Fatalf("weekly slot = %v", got)
	}
	for name, rep := range map[string]string{
		"unknown type":    "  - {type: daily}",
		"twice":           "  - {type: period}\n  - {type: period}",
		"quota no levels": "  - {type: traffic_quota}",
		"quota unsorted":  "  - {type: traffic_quota, levels: [90, 80]}",
		"expiry no days":  "  - {type: expiry}",
		"expiry fraction": "  - {type: expiry, days: [1.5]}",
		"expiry twice":    "  - {type: expiry, days: [7, 1, 7]}",
		"expiry negative": "  - {type: expiry, days: [-1]}",
		"expiry levels":   "  - {type: expiry, levels: [7]}",
		"ip_change days":  "  - {type: ip_change, days: [1]}",
		"weekly no at":    "  - {type: weekly}",
		"weekly bad day":  "  - {type: weekly, at: 'Monday 09:00'}",
		"weekly bad time": "  - {type: weekly, at: 'Mon 9am'}",
		"at on period":    "  - {type: period, at: 'Mon 09:00'}",
		"unknown node":    "  - {type: period, nodes: [zz]}",
		"unknown key":     "  - {type: period, for: 1m}",
	} {
		if _, err := Parse([]byte(head + rep + "\n")); err == nil {
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

func TestNotify(t *testing.T) {
	base := "nodes:\n  - {id: a, token: " + tokA + "}\n"
	c, err := Parse([]byte(base + `notify:
  - {type: telegram, bot_token: "1:a", chat_id: -1001234567890}
  - type: webhook
    name: bark
    url: "https://api.day.app/KEY/{{title}}/{{message}}"
    method: get
  - type: webhook
    name: discord
    url: https://discord.com/api/webhooks/1/abc
    headers: {Content-Type: application/json}
    body: '{"content": {{message}}}'
  - {type: telegram, name: ops-group, bot_token: "2:b", chat_id: 7}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ChannelNames(), ","); got != "telegram,bark,discord,ops-group" {
		t.Fatalf("channels = %s", got)
	}
	if c.Notify[0].ChatID != "-1001234567890" || c.Notify[1].Method != "GET" || c.Notify[2].Method != "POST" {
		t.Fatalf("channels: %+v", c.Notify)
	}
	if c, _ := Parse([]byte(base)); len(c.ChannelNames()) != 0 || c.ChannelNames() == nil {
		t.Fatalf("no channels: %#v", c.ChannelNames())
	}
	for name, bad := range map[string]string{
		"no type":         "notify: [{name: a, url: \"https://x.example/a\"}]",
		"unknown type":    "notify: [{type: email, name: a}]",
		"no name":         "notify: [{type: webhook, url: \"https://x.example/a\"}]",
		"bad name":        "notify: [{type: webhook, name: \"a b\", url: \"https://x.example/a\"}]",
		"duplicate name":  "notify: [{type: webhook, name: a, url: \"https://x.example/a\"}, {type: webhook, name: a, url: \"https://x.example/b\"}]",
		"two telegrams":   "notify: [{type: telegram, bot_token: '1:a', chat_id: 5}, {type: telegram, bot_token: '2:b', chat_id: 6}]",
		"half telegram":   "notify: [{type: telegram, bot_token: '1:a'}]",
		"telegram url":    "notify: [{type: telegram, bot_token: '1:a', chat_id: 5, url: \"https://x.example/a\"}]",
		"webhook chat_id": "notify: [{type: webhook, name: a, url: \"https://x.example/a\", chat_id: 5}]",
		"no url":          "notify: [{type: webhook, name: a}]",
		"not http":        "notify: [{type: webhook, name: a, url: \"file:///etc/passwd\"}]",
		"no host":         "notify: [{type: webhook, name: a, url: \"https:///x\"}]",
		"bad method":      "notify: [{type: webhook, name: a, url: \"https://x.example/a\", method: DELETE}]",
		"GET with body":   "notify: [{type: webhook, name: a, url: \"https://x.example/a\", method: GET, body: x}]",
		"header newline":  "notify: [{type: webhook, name: a, url: \"https://x.example/a\", headers: {X: \"a\\nb\"}}]",
		"unknown key":     "notify: [{type: webhook, name: a, url: \"https://x.example/a\", exec: id}]",
	} {
		if _, err := Parse([]byte(base + bad + "\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	base := "nodes:\n  - {id: a, token: " + tokA + "}\n"
	for _, ok := range []string{"probe.example.com:9527", "203.0.113.1:9527", "[2001:db8::1]:9527"} {
		if c, err := Parse([]byte(base + "public_addr: \"" + ok + "\"\n")); err != nil || c.PublicAddr != ok {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"probe.example.com", "probe.example.com:0", "probe.example.com:x", ":9527", "a b:9527"} {
		if _, err := Parse([]byte(base + "public_addr: \"" + bad + "\"\n")); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	base := "nodes:\n  - {id: a, token: " + tokA + "}\n"
	// Only the format and cost are checked here.
	hash := "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	c, err := Parse([]byte(base + "basic_auth: {user: admin, password_hash: \"" + hash + "\"}\n"))
	if err != nil || !c.BasicAuth.Enabled() {
		t.Fatal(err)
	}
	if c, err := Parse([]byte(base)); err != nil || c.BasicAuth.Enabled() {
		t.Fatalf("basic_auth on without being configured: %v", err)
	}
	aud := "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2"
	for _, bad := range []string{
		"basic_auth: {user: admin}",
		"basic_auth: {password_hash: \"" + hash + "\"}",
		"basic_auth: {user: admin, password_hash: secret}", // a plaintext password
		"basic_auth: {user: admin, password_hash: \"$2a$04$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy\"}", // cost too low
		"basic_auth: {user: \"a:b\", password_hash: \"" + hash + "\"}",
		"basic_auth: {user: admin, password_hash: \"" + hash + "\"}\ncf_access: {team_domain: myteam.cloudflareaccess.com, aud: " + aud + "}",
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
		pl := Plan{ExpireAt: c.expire, RenewMonths: c.renew}
		pl.expire, _ = time.Parse(time.DateOnly, c.expire)
		date, days, ok := pl.Expiry(c.now, sh)
		if !ok || date != c.date || days != c.days {
			t.Errorf("%s every %d at %s: %s, %d days; want %s, %d", c.expire, c.renew, c.now, date, days, c.date, c.days)
		}
	}
	if _, _, ok := (&Plan{}).Expiry(time.Now(), sh); ok {
		t.Error("no expire_at: ok")
	}
	c, err := Parse([]byte("nodes:\n  - {id: a, token: " + tokA + ", plan: {expire_at: 2027-03-15, renew_months: 12, price: $10/年}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if date, _, ok := c.Nodes[0].Plan.Expiry(at("2026-10-01T00:00:00Z"), c.Location); !ok || date != "2027-03-15" || c.Nodes[0].Plan.Price != "$10/年" {
		t.Fatalf("parsed: %s %+v", date, c.Nodes[0])
	}
}

func TestQuota(t *testing.T) {
	for mode, want := range map[string]int64{QuotaSum: 40, QuotaMax: 30, QuotaTX: 10, QuotaRX: 30} {
		if got := (Traffic{QuotaMode: mode}).Billable(30, 10); got != want {
			t.Errorf("%s: billable = %d, want %d", mode, got, want)
		}
	}
	if q := (Traffic{QuotaGB: 1.5}).Quota(); q != 3<<29 {
		t.Errorf("quota = %d", q)
	}
}
