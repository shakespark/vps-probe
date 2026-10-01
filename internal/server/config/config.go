// Package config loads the server's YAML configuration. Nodes exist only
// here: nothing registers a node at runtime.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vpsprobe/internal/wire"
)

type Config struct {
	Listen    Listen    `yaml:"listen"`
	DB        string    `yaml:"db"`
	Timezone  string    `yaml:"timezone"`
	Nodes     []Node    `yaml:"nodes"`
	Retention Retention `yaml:"retention"`
	Backup    Backup    `yaml:"backup"`
	// A node is shown offline when no fresh report arrived for this long.
	// Keep it at about 3x the agents' interval.
	OfflineAfter Duration `yaml:"offline_after"`
	Telegram     Telegram `yaml:"telegram"`
	CFAccess     CFAccess `yaml:"cf_access"`
	// nil (key absent) means DefaultRules; an empty list disables alerts.
	Alerts []Rule `yaml:"alerts"`

	Location *time.Location `yaml:"-"`
}

type Listen struct {
	Web    string `yaml:"web"`    // HTTP, meant for cloudflared on loopback
	Ingest string `yaml:"ingest"` // UDP, agents report here
}

type Node struct {
	ID        string  `yaml:"id"`
	Name      string  `yaml:"name"`
	Token     string  `yaml:"token"`
	QuotaGB   float64 `yaml:"traffic_quota_gb"`
	QuotaMode string  `yaml:"traffic_quota_mode"` // sum | max | tx | rx
	// Only used by agent-config: the address other agents ping, and this
	// node's traffic reset day and time. The agent's own file stays
	// authoritative.
	Addr       string `yaml:"addr"`
	ResetDay   int    `yaml:"reset_day"`
	ResetTime  string `yaml:"reset_time"` // HH:MM, default 00:00
	DisplayIdx int    `yaml:"-"`          // position in the file
}

type Retention struct {
	Raw Duration `yaml:"raw"`
	M5  Duration `yaml:"m5"`
	H1  Duration `yaml:"h1"`
}

type Backup struct {
	Dir  string `yaml:"dir"`
	Keep int    `yaml:"keep"` // 0 disables the daily backup
}

// CFAccess, when set, makes the web UI require a valid Cloudflare Access
// JWT on every request.
type CFAccess struct {
	TeamDomain string `yaml:"team_domain"` // myteam.cloudflareaccess.com
	AUD        string `yaml:"aud"`         // Application Audience (AUD) tag
}

func (c CFAccess) Enabled() bool { return c.TeamDomain != "" }

// Telegram is optional; without it alerts are only logged and recorded.
type Telegram struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"` // a group id like -100123 may be written unquoted
}

func (t Telegram) Enabled() bool { return t.BotToken != "" }

// Rule is one alert rule; see DESIGN.md §7.
type Rule struct {
	Name           string    `yaml:"name" json:"name"`
	Metric         string    `yaml:"metric" json:"metric"`
	Op             string    `yaml:"op" json:"op,omitempty"`
	Threshold      *float64  `yaml:"threshold" json:"threshold,omitempty"`
	For            Duration  `yaml:"for" json:"for"`
	Nodes          NodeSet   `yaml:"nodes" json:"nodes"`
	Repeat         Duration  `yaml:"repeat" json:"repeat"`
	NotifyRecovery *bool     `yaml:"notify_recovery" json:"notify_recovery"`
	Levels         []float64 `yaml:"levels" json:"levels,omitempty"`
}

// Metrics a rule can watch.
const (
	MetricCPU      = "cpu"
	MetricSteal    = "steal"
	MetricLoad1    = "load1"
	MetricMem      = "mem"
	MetricSwap     = "swap"
	MetricDisk     = "disk"
	MetricOffline  = "offline"
	MetricPingLoss = "ping_loss"
	MetricPingAvg  = "ping_avg"
	MetricTraffic  = "traffic"
)

var (
	metrics = []string{MetricCPU, MetricSteal, MetricLoad1, MetricMem, MetricSwap, MetricDisk,
		MetricOffline, MetricPingLoss, MetricPingAvg, MetricTraffic}
	ops = []string{">", ">=", "<", "<="}
)

// Recovers reports whether recovery messages are sent (default true).
func (r *Rule) Recovers() bool { return r.NotifyRecovery == nil || *r.NotifyRecovery }

// NodeSet is either "all" (the default) or a list of node ids.
type NodeSet struct {
	IDs []string // nil means all
}

func (n *NodeSet) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		if v.Value != "all" {
			return fmt.Errorf("nodes: want \"all\" or a list of node ids, got %q", v.Value)
		}
		n.IDs = nil
		return nil
	}
	var ids []string
	if err := v.Decode(&ids); err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("nodes: empty list; omit it or write \"all\"")
	}
	n.IDs = ids
	return nil
}

func (n NodeSet) MarshalJSON() ([]byte, error) {
	if n.IDs == nil {
		return []byte(`"all"`), nil
	}
	return json.Marshal(n.IDs)
}

func (n NodeSet) Has(id string) bool { return n.IDs == nil || slices.Contains(n.IDs, id) }

// String drops zero trailing units: 5m, 1h, 90s rather than 5m0s, 1h0m0s.
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func ptr(v float64) *float64 { return &v }

// DefaultRules apply when the config has no alerts key.
func DefaultRules() []Rule {
	return []Rule{
		{Name: "offline", Metric: MetricOffline, For: Duration(time.Minute)},
		{Name: "cpu_high", Metric: MetricCPU, Op: ">", Threshold: ptr(90), For: Duration(5 * time.Minute)},
		{Name: "mem_high", Metric: MetricMem, Op: ">", Threshold: ptr(90), For: Duration(5 * time.Minute)},
		{Name: "disk_full", Metric: MetricDisk, Op: ">", Threshold: ptr(90), For: Duration(10 * time.Minute)},
		{Name: "link_loss", Metric: MetricPingLoss, Op: ">", Threshold: ptr(20), For: Duration(3 * time.Minute)},
		{Name: "traffic_quota", Metric: MetricTraffic, Levels: []float64{80, 90, 100}},
	}
}

// Duration accepts Go durations plus a "d" (day) suffix, e.g. "400d".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

const MinTokenLen = 32

var quotaModes = []string{"sum", "max", "tx", "rx"}

// Load reads the config file. It holds every node's token, so it must not
// be readable by others or writable by the group: root:vps-probe-server 0640
// lets the service read it without being able to change it.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if os.IsPermission(err) {
		return nil, fmt.Errorf("%w (the service user needs read access to the file and to every directory above it; "+
			"expected /etc/vps-probe 0755, server.yml root:vps-probe-server 0640)", err)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if perm := st.Mode().Perm(); perm&0o037 != 0 {
		return nil, fmt.Errorf("config: %s has mode %04o; it contains tokens, run: chmod 640 %s", path, perm, path)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return Parse(buf.Bytes())
}

func Parse(data []byte) (*Config, error) {
	c := &Config{
		Listen:   Listen{Web: "127.0.0.1:8080", Ingest: ":9527"}, // empty host: IPv4 and IPv6
		DB:       "/var/lib/vps-probe-server/probe.db",
		Timezone: "Asia/Shanghai",
		Retention: Retention{
			Raw: Duration(48 * time.Hour),
			M5:  Duration(30 * 24 * time.Hour),
			H1:  Duration(400 * 24 * time.Hour),
		},
		Backup:       Backup{Dir: "/var/lib/vps-probe-server/backups", Keep: 7},
		OfflineAfter: Duration(30 * time.Second),
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for name, addr := range map[string]string{"listen.web": c.Listen.Web, "listen.ingest": c.Listen.Ingest} {
		if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
			bad("%s %q: want host:port", name, addr)
		}
	}
	if !filepath.IsAbs(c.DB) {
		bad("db %q: must be an absolute path", c.DB)
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		bad("timezone %q: %v", c.Timezone, err)
	}
	c.Location = loc

	if len(c.Nodes) == 0 {
		bad("nodes: at least one node is required")
	}
	ids := map[string]bool{}
	tokens := map[string]string{}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		n.DisplayIdx = i
		if !wire.ValidNode(n.ID) {
			bad("nodes[%d].id %q: must be 1-%d chars of letters, digits, '.', '_', '-'", i, n.ID, wire.MaxNodeLen)
		}
		if ids[n.ID] {
			bad("nodes: duplicate id %q", n.ID)
		}
		ids[n.ID] = true
		if n.Name == "" {
			n.Name = n.ID
		}
		if len(n.Token) < MinTokenLen {
			bad("nodes[%s].token: must be at least %d characters (use gen-token)", n.ID, MinTokenLen)
		} else if other, dup := tokens[n.Token]; dup {
			bad("nodes[%s].token: same as %s; every node needs its own token", n.ID, other)
		}
		tokens[n.Token] = n.ID
		if n.Addr != "" && !validHost(n.Addr) {
			bad("nodes[%s].addr %q: want an IP address or hostname, without port", n.ID, n.Addr)
		}
		if n.ResetDay == 0 {
			n.ResetDay = 1
		}
		if n.ResetDay < 1 || n.ResetDay > 31 {
			bad("nodes[%s].reset_day: must be 1-31", n.ID)
		}
		if n.ResetTime != "" {
			if t, err := time.Parse("15:04", n.ResetTime); err != nil {
				bad("nodes[%s].reset_time %q: want HH:MM (24-hour)", n.ID, n.ResetTime)
			} else {
				n.ResetTime = t.Format("15:04")
			}
		}
		if n.QuotaGB < 0 {
			bad("nodes[%s].traffic_quota_gb: must not be negative", n.ID)
		}
		if n.QuotaMode == "" {
			n.QuotaMode = "sum"
		}
		if !slices.Contains(quotaModes, n.QuotaMode) {
			bad("nodes[%s].traffic_quota_mode %q: want one of %v", n.ID, n.QuotaMode, quotaModes)
		}
	}

	if time.Duration(c.OfflineAfter) < 5*time.Second {
		bad("offline_after: must be at least 5s")
	}

	if (c.Telegram.BotToken == "") != (c.Telegram.ChatID == "") {
		bad("telegram: set both bot_token and chat_id, or neither")
	}
	if a := c.CFAccess; a.TeamDomain != "" || a.AUD != "" {
		team, ok := strings.CutSuffix(a.TeamDomain, ".cloudflareaccess.com")
		if !ok || team == "" || !validHost(a.TeamDomain) {
			bad("cf_access.team_domain %q: want <team>.cloudflareaccess.com (Zero Trust → Settings → Custom Pages)", a.TeamDomain)
		}
		if len(a.AUD) != 64 || strings.Trim(strings.ToLower(a.AUD), "0123456789abcdef") != "" {
			bad("cf_access.aud: want the 64-hex-character Application Audience (AUD) tag of the Access application")
		}
	}
	if c.Alerts == nil {
		c.Alerts = DefaultRules()
	}
	names := map[string]bool{}
	for i := range c.Alerts {
		r := &c.Alerts[i]
		where := fmt.Sprintf("alerts[%d] %q", i, r.Name)
		if r.Name == "" || len(r.Name) > 64 {
			bad("alerts[%d].name: required, at most 64 characters", i)
		}
		if names[r.Name] {
			bad("%s: duplicate name", where)
		}
		names[r.Name] = true
		if !slices.Contains(metrics, r.Metric) {
			bad("%s: metric %q: want one of %v", where, r.Metric, metrics)
		}
		for _, id := range r.Nodes.IDs {
			if _, ok := ids[id]; !ok {
				bad("%s: nodes: %q is not a configured node", where, id)
			}
		}
		if r.For < 0 || r.Repeat < 0 {
			bad("%s: for/repeat must not be negative", where)
		}
		if r.Repeat > 0 && time.Duration(r.Repeat) < time.Minute {
			bad("%s: repeat: at least 1m, or 0 for no reminders", where)
		}
		switch r.Metric {
		case MetricOffline:
			if r.Op != "" || r.Threshold != nil || r.Levels != nil {
				bad("%s: offline takes only for (how long without reports)", where)
			}
			if time.Duration(r.For) < 10*time.Second {
				bad("%s: offline needs for >= 10s", where)
			}
		case MetricTraffic:
			if r.Op != "" || r.Threshold != nil {
				bad("%s: traffic takes levels (percent of quota), not op/threshold", where)
			}
			if len(r.Levels) == 0 || !slices.IsSorted(r.Levels) || r.Levels[0] <= 0 || r.Levels[len(r.Levels)-1] > 1000 {
				bad("%s: levels: want ascending percentages, e.g. [80, 90, 100]", where)
			}
		default:
			if !slices.Contains(ops, r.Op) {
				bad("%s: op %q: want one of %v", where, r.Op, ops)
			}
			if r.Threshold == nil {
				bad("%s: threshold is required", where)
			}
			if r.Levels != nil {
				bad("%s: levels only apply to traffic", where)
			}
		}
	}

	r := c.Retention
	if time.Duration(r.Raw) < 6*time.Hour {
		bad("retention.raw: must be at least 6h (rollups recompute the last 3h from raw)")
	}
	if r.M5 < r.Raw || r.H1 < r.M5 {
		bad("retention: want raw <= m5 <= h1")
	}
	if c.Backup.Keep < 0 {
		bad("backup.keep: must not be negative")
	}
	if c.Backup.Keep > 0 && !filepath.IsAbs(c.Backup.Dir) {
		bad("backup.dir %q: must be an absolute path", c.Backup.Dir)
	}
	return errors.Join(errs...)
}

func validHost(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Node looks up a node by id.
func (c *Config) Node(id string) (*Node, bool) {
	for i := range c.Nodes {
		if c.Nodes[i].ID == id {
			return &c.Nodes[i], true
		}
	}
	return nil, false
}
