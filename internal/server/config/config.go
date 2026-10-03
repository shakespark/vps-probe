// Package config loads the server's YAML configuration. Nodes exist only
// here: nothing registers a node at runtime.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"vpsprobe/internal/echo"
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
	Addr      string `yaml:"addr"`
	ResetDay  int    `yaml:"reset_day"`
	ResetTime string `yaml:"reset_time"` // HH:MM, default 00:00
	// Node ids this node and those nodes don't ping, in either direction.
	NoPing []string `yaml:"no_ping"`
	// Non-node targets this node pings, e.g. 1.1.1.1 through a tunnel.
	ExtraPeers []ExtraPeer `yaml:"extra_peers"`
	// Plan details, shown on the overview and used by expiry rules.
	ExpireAt    string `yaml:"expire_at"`    // YYYY-MM-DD in the server timezone
	RenewMonths int    `yaml:"renew_months"` // auto-renewing plan: a passed expiry moves forward by this many months
	Price       string `yaml:"price"`        // display only, e.g. "$10/年"
	// Display only: a short region code shown as a badge (HK, JP, US-LA)
	// and a group for the overview tabs. Order on the page is file order.
	Region     string `yaml:"region"`
	Group      string `yaml:"group"`
	DisplayIdx int    `yaml:"-"` // position in the file

	expire time.Time // ExpireAt parsed, midnight UTC (a calendar date)
}

const (
	MaxPriceLen  = 64
	MaxRegionLen = 8
	MaxGroupLen  = 32
)

// ExtraPeer is copied into the node's agent.yml ping.peers as is.
type ExtraPeer struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
	Type string `yaml:"type"` // icmp (default) | dns | echo; addr is host:port for dns and echo
	Key  string `yaml:"key"`  // echo: the responder's key
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
	Exclude        []string  `yaml:"exclude" json:"exclude,omitempty"` // with nodes: all
	Repeat         Duration  `yaml:"repeat" json:"repeat"`
	NotifyRecovery *bool     `yaml:"notify_recovery" json:"notify_recovery"`
	Levels         []float64 `yaml:"levels" json:"levels,omitempty"`
	// net_in / net_out: also require this direction >= Ratio x the other
	// one; 0 = no such check.
	Ratio float64 `yaml:"ratio" json:"ratio,omitempty"`
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
	MetricExpiry   = "expiry"
	MetricIPChange = "ip_change"
	MetricNetIn    = "net_in"  // Mbps, summed over the reported interfaces
	MetricNetOut   = "net_out" // Mbps
)

var (
	metrics = []string{MetricCPU, MetricSteal, MetricLoad1, MetricMem, MetricSwap, MetricDisk,
		MetricOffline, MetricPingLoss, MetricPingAvg, MetricTraffic, MetricExpiry, MetricIPChange,
		MetricNetIn, MetricNetOut}
	ops = []string{">", ">=", "<", "<="}
)

// Covers reports whether the rule applies to node id.
func (r *Rule) Covers(id string) bool { return r.Nodes.Has(id) && !slices.Contains(r.Exclude, id) }

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
		{Name: "expiry", Metric: MetricExpiry, Levels: []float64{7, 1}},
		{Name: "ddos", Metric: MetricNetIn, Op: ">=", Threshold: ptr(50), Ratio: 4, For: Duration(2 * time.Minute)},
		{Name: "abuse_out", Metric: MetricNetOut, Op: ">=", Threshold: ptr(50), Ratio: 4, For: Duration(5 * time.Minute)},
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
		if n.ExpireAt != "" {
			if t, err := time.Parse(time.DateOnly, n.ExpireAt); err != nil {
				bad("nodes[%s].expire_at %q: want YYYY-MM-DD", n.ID, n.ExpireAt)
			} else {
				n.expire = t
			}
		}
		if n.RenewMonths < 0 || n.RenewMonths > 120 {
			bad("nodes[%s].renew_months: must be 0-120", n.ID)
		} else if n.RenewMonths > 0 && n.ExpireAt == "" {
			bad("nodes[%s].renew_months: needs expire_at", n.ID)
		}
		if len([]rune(n.Price)) > MaxPriceLen || strings.ContainsFunc(n.Price, unicode.IsControl) {
			bad("nodes[%s].price: at most %d characters on one line", n.ID, MaxPriceLen)
		}
		n.Region = strings.ToUpper(n.Region)
		if len(n.Region) > MaxRegionLen || strings.ContainsFunc(n.Region, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
		}) {
			bad("nodes[%s].region %q: at most %d letters, digits or '-', e.g. HK or US-LA", n.ID, n.Region, MaxRegionLen)
		}
		if len([]rune(n.Group)) > MaxGroupLen || strings.ContainsFunc(n.Group, unicode.IsControl) {
			bad("nodes[%s].group: at most %d characters on one line", n.ID, MaxGroupLen)
		}
	}

	for _, n := range c.Nodes {
		seen := map[string]bool{}
		for _, p := range n.NoPing {
			switch {
			case !ids[p]:
				bad("nodes[%s].no_ping: %q is not a node id", n.ID, p)
			case p == n.ID:
				bad("nodes[%s].no_ping: lists the node itself", n.ID)
			case seen[p]:
				bad("nodes[%s].no_ping: %q listed twice", n.ID, p)
			}
			seen[p] = true
		}
		for _, p := range n.ExtraPeers {
			switch {
			case !wire.ValidNode(p.Name):
				bad("nodes[%s].extra_peers: name %q: must be 1-%d chars of letters, digits, '.', '_', '-'", n.ID, p.Name, wire.MaxNodeLen)
			case ids[p.Name]:
				bad("nodes[%s].extra_peers: %q is a node id; its latency would mix into that node's column", n.ID, p.Name)
			case seen[p.Name]:
				bad("nodes[%s].extra_peers: %q listed twice", n.ID, p.Name)
			case p.Type == "" || p.Type == "icmp":
				if !validHost(p.Addr) {
					bad("nodes[%s].extra_peers[%s].addr %q: want an IP address or hostname, without port", n.ID, p.Name, p.Addr)
				}
			case p.Type == "dns" || p.Type == "echo":
				host, port, err := net.SplitHostPort(p.Addr)
				if pn, perr := strconv.ParseUint(port, 10, 16); err != nil || perr != nil || pn == 0 || !validHost(host) {
					bad("nodes[%s].extra_peers[%s].addr %q: a %s peer wants host:port", n.ID, p.Name, p.Addr, p.Type)
				}
			default:
				bad("nodes[%s].extra_peers[%s].type %q: want icmp, dns or echo", n.ID, p.Name, p.Type)
			}
			if p.Type == "echo" && len(p.Key) < echo.MinKeyLen {
				bad("nodes[%s].extra_peers[%s].key: an echo peer needs the responder's key, at least %d characters", n.ID, p.Name, echo.MinKeyLen)
			} else if p.Type != "echo" && p.Key != "" {
				bad("nodes[%s].extra_peers[%s].key: only echo peers take a key", n.ID, p.Name)
			}
			seen[p.Name] = true
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
		if r.Exclude != nil {
			if r.Nodes.IDs != nil {
				bad("%s: exclude only goes with nodes: all; drop the node from the nodes list instead", where)
			}
			if len(r.Exclude) == 0 {
				bad("%s: exclude: empty list; omit it", where)
			}
			for _, id := range r.Exclude {
				if _, ok := ids[id]; !ok {
					bad("%s: exclude: %q is not a configured node", where, id)
				}
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
		case MetricExpiry:
			if r.Op != "" || r.Threshold != nil || r.For != 0 || r.Repeat != 0 {
				bad("%s: expiry takes levels (days before expiry), not op/threshold/for/repeat", where)
			}
			if len(r.Levels) == 0 || slices.ContainsFunc(r.Levels, func(l float64) bool { return l < 0 || l > 365 || l != math.Trunc(l) }) ||
				len(slices.Compact(slices.Sorted(slices.Values(r.Levels)))) != len(r.Levels) {
				bad("%s: levels: want distinct whole days 0-365, e.g. [7, 1] (0 = on the day)", where)
			}
		case MetricIPChange:
			if r.Op != "" || r.Threshold != nil || r.Levels != nil || r.For != 0 || r.Repeat != 0 {
				bad("%s: ip_change takes only nodes", where)
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
		if r.Ratio != 0 {
			switch {
			case r.Metric != MetricNetIn && r.Metric != MetricNetOut:
				bad("%s: ratio only applies to net_in / net_out", where)
			case r.Ratio < 1:
				bad("%s: ratio: want >= 1 (this direction at least ratio x the other), or omit it", where)
			case r.Op != ">" && r.Op != ">=":
				bad("%s: ratio needs op > or >=", where)
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

// NoPing reports whether a and b must not ping each other: either one
// lists the other in no_ping.
func (c *Config) NoPing(a, b string) bool {
	na, okA := c.Node(a)
	nb, okB := c.Node(b)
	return okA && slices.Contains(na.NoPing, b) || okB && slices.Contains(nb.NoPing, a)
}

// Expiry returns the node's next expiry date as of now and the whole days
// left until it, counted in loc's calendar (0 = today, negative = past).
// With renew_months, a passed date moves forward by whole cycles, each
// counted from expire_at and clamped to the end of shorter months.
// ok is false when the node has no expire_at.
func (n *Node) Expiry(now time.Time, loc *time.Location) (date string, days int, ok bool) {
	if n.expire.IsZero() {
		return "", 0, false
	}
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	exp := n.expire
	for k := 1; n.RenewMonths > 0 && exp.Before(today); k++ {
		exp = addMonths(n.expire, k*n.RenewMonths)
	}
	return exp.Format(time.DateOnly), int(exp.Sub(today).Hours() / 24), true
}

// addMonths keeps t's day of month, or the month's last day if it is shorter.
func addMonths(t time.Time, months int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(t.Day(), last)-1)
}
