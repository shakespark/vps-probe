package config

import (
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/shakespark/vps-probe/internal/netaddr"
	"github.com/shakespark/vps-probe/internal/peer"
	"github.com/shakespark/vps-probe/internal/wire"
)

// Node is one monitored machine. Its position in the file is its position
// on the page.
type Node struct {
	ID    string `yaml:"id"`
	Name  string `yaml:"name"` // shown instead of the id; the id when omitted
	Token string `yaml:"token"`
	// Display only: a short region code shown as a badge (HK, JP, US-LA) and
	// a group for the overview tabs.
	Region string `yaml:"region"`
	Group  string `yaml:"group"`

	Traffic Traffic `yaml:"traffic"`
	Plan    Plan    `yaml:"plan"`
	Ping    Ping    `yaml:"ping"`

	// What the agent measures when its own defaults don't fit. Like the
	// period settings these are only written into the node's agent.yml.
	Disks      []string `yaml:"disks"`      // mount points; none = the agent's default, /
	Interfaces []string `yaml:"interfaces"` // counted for traffic; none = the agent detects them
}

// Traffic is the node's data allowance and when its billing period begins.
// The period settings are not used by the server: they are written into the
// node's agent.yml, and the agent reports the periods it counts.
type Traffic struct {
	QuotaGB   float64 `yaml:"quota_gb"`   // GiB; 0 = unlimited
	QuotaMode string  `yaml:"quota_mode"` // what counts against it: sum | max | tx | rx
	ResetDay  int     `yaml:"reset_day"`
	ResetTime string  `yaml:"reset_time"` // HH:MM
}

// Plan is what the node costs and until when it is paid, shown on the
// overview and watched by the expiry report.
type Plan struct {
	ExpireAt    string `yaml:"expire_at"`    // YYYY-MM-DD in the server timezone
	RenewMonths int    `yaml:"renew_months"` // auto-renewing: a passed date moves forward by this many months
	Price       string `yaml:"price"`        // display only, e.g. "$10/年"

	expire time.Time // ExpireAt parsed, midnight UTC (a calendar date)
}

// Ping is the node's place in the latency matrix. Like the period settings
// it only shapes generated agent configs: every agent pings what its own
// file says.
type Ping struct {
	Addr    string      `yaml:"addr"`    // where other nodes ping this one; none = they don't
	Exclude []string    `yaml:"exclude"` // node ids this node and those nodes don't ping, in either direction
	Extra   []peer.Peer `yaml:"extra"`   // targets that are not nodes, e.g. 1.1.1.1 through a tunnel
}

const (
	MaxPriceLen  = 64
	MaxRegionLen = 8
	MaxGroupLen  = 32
)

// Quota modes: what of a period's traffic counts against the quota.
const (
	QuotaSum = "sum" // received + sent
	QuotaMax = "max" // the larger of the two
	QuotaTX  = "tx"
	QuotaRX  = "rx"
)

var quotaModes = []string{QuotaSum, QuotaMax, QuotaTX, QuotaRX}

// QuotaModeText names the quota mode for people.
func (t Traffic) QuotaModeText() string {
	return map[string]string{QuotaSum: "收+发", QuotaMax: "取大", QuotaTX: "仅上行", QuotaRX: "仅下行"}[t.QuotaMode]
}

// Quota returns the allowance in bytes, 0 if unlimited.
func (t Traffic) Quota() int64 { return int64(t.QuotaGB * (1 << 30)) }

// Billable returns what rx received and tx sent bytes count against the
// quota.
func (t Traffic) Billable(rx, tx int64) int64 {
	switch t.QuotaMode {
	case QuotaMax:
		return max(rx, tx)
	case QuotaTX:
		return tx
	case QuotaRX:
		return rx
	}
	return rx + tx
}

// validateNodes checks the nodes, fills in their defaults and returns the
// set of node ids. A config without nodes is valid: that is a server just
// installed, before its first add-node.
func (c *Config) validateNodes(p *problems) map[string]bool {
	ids := map[string]bool{}
	tokens := map[string]string{}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if !wire.ValidID(n.ID) {
			p.add("nodes[%d].id %q: must be 1-%d chars of letters, digits, '.', '_', '-'", i, n.ID, wire.MaxIDLen)
		}
		if ids[n.ID] {
			p.add("nodes: duplicate id %q", n.ID)
		}
		ids[n.ID] = true
		if n.Name == "" {
			n.Name = n.ID
		}
		if len(n.Token) < wire.MinTokenLen {
			p.add("nodes[%s].token: must be at least %d characters (use gen-token)", n.ID, wire.MinTokenLen)
		} else if other, dup := tokens[n.Token]; dup {
			p.add("nodes[%s].token: same as %s; every node needs its own token", n.ID, other)
		}
		tokens[n.Token] = n.ID

		n.Region = strings.ToUpper(n.Region)
		if len(n.Region) > MaxRegionLen || strings.ContainsFunc(n.Region, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
		}) {
			p.add("nodes[%s].region %q: at most %d letters, digits or '-', e.g. HK or US-LA", n.ID, n.Region, MaxRegionLen)
		}
		if !oneLine(n.Group, MaxGroupLen) {
			p.add("nodes[%s].group: at most %d characters on one line", n.ID, MaxGroupLen)
		}
		n.Traffic.validate(p, n.ID)
		n.Plan.validate(p, n.ID)
		// The same checks as the agent's, so a generated agent.yml loads.
		for _, d := range n.Disks {
			if !path.IsAbs(d) {
				p.add("nodes[%s].disks: %q is not an absolute path", n.ID, d)
			}
		}
		for _, x := range n.Interfaces {
			if x == "" || strings.ContainsAny(x, "/ ") {
				p.add("nodes[%s].interfaces: invalid name %q", n.ID, x)
			}
		}
	}
	// Ping settings name other nodes, so they are checked once all ids are known.
	for i := range c.Nodes {
		c.Nodes[i].Ping.validate(p, c.Nodes[i].ID, ids)
	}
	return ids
}

func oneLine(s string, maxLen int) bool {
	return len([]rune(s)) <= maxLen && !strings.ContainsFunc(s, unicode.IsControl)
}

func (t *Traffic) validate(p *problems, id string) {
	if t.QuotaGB < 0 {
		p.add("nodes[%s].traffic.quota_gb: must not be negative", id)
	}
	if t.QuotaMode == "" {
		t.QuotaMode = QuotaSum
	}
	if !slices.Contains(quotaModes, t.QuotaMode) {
		p.add("nodes[%s].traffic.quota_mode %q: want one of %v", id, t.QuotaMode, quotaModes)
	}
	if t.ResetDay == 0 {
		t.ResetDay = 1
	}
	if t.ResetDay < 1 || t.ResetDay > 31 {
		p.add("nodes[%s].traffic.reset_day: must be 1-31", id)
	}
	if t.ResetTime == "" {
		t.ResetTime = "00:00"
	}
	if at, err := time.Parse("15:04", t.ResetTime); err != nil {
		p.add("nodes[%s].traffic.reset_time %q: want HH:MM (24-hour)", id, t.ResetTime)
	} else {
		t.ResetTime = at.Format("15:04")
	}
}

func (pl *Plan) validate(p *problems, id string) {
	if pl.ExpireAt != "" {
		t, err := time.Parse(time.DateOnly, pl.ExpireAt)
		if err != nil {
			p.add("nodes[%s].plan.expire_at %q: want YYYY-MM-DD", id, pl.ExpireAt)
		}
		pl.expire = t
	}
	if pl.RenewMonths < 0 || pl.RenewMonths > 120 {
		p.add("nodes[%s].plan.renew_months: must be 0-120", id)
	} else if pl.RenewMonths > 0 && pl.ExpireAt == "" {
		p.add("nodes[%s].plan.renew_months: needs expire_at", id)
	}
	if !oneLine(pl.Price, MaxPriceLen) {
		p.add("nodes[%s].plan.price: at most %d characters on one line", id, MaxPriceLen)
	}
}

func (g *Ping) validate(p *problems, id string, ids map[string]bool) {
	if g.Addr != "" && !netaddr.ValidHost(g.Addr) {
		p.add("nodes[%s].ping.addr %q: want an IP address or host name, without port", id, g.Addr)
	}
	seen := map[string]bool{}
	for _, x := range g.Exclude {
		switch {
		case !ids[x]:
			p.add("nodes[%s].ping.exclude: %q is not a node id", id, x)
		case x == id:
			p.add("nodes[%s].ping.exclude: lists the node itself", id)
		case seen[x]:
			p.add("nodes[%s].ping.exclude: %q listed twice", id, x)
		}
		seen[x] = true
	}
	for i := range g.Extra {
		x := &g.Extra[i]
		switch err := x.Validate(); {
		case err != nil:
			p.add("nodes[%s].ping.extra: %w", id, err)
		case ids[x.Name]:
			p.add("nodes[%s].ping.extra: %q is a node id; its latency would mix into that node's column", id, x.Name)
		case seen[x.Name]:
			p.add("nodes[%s].ping.extra: %q listed twice", id, x.Name)
		}
		seen[x.Name] = true
	}
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

// PingExcluded reports whether a and b must not ping each other: either one
// lists the other in ping.exclude.
func (c *Config) PingExcluded(a, b string) bool {
	na, okA := c.Node(a)
	nb, okB := c.Node(b)
	return okA && slices.Contains(na.Ping.Exclude, b) || okB && slices.Contains(nb.Ping.Exclude, a)
}

// Expiry returns the plan's next expiry date as of now and the whole days
// left until it, counted in loc's calendar (0 = today, negative = past).
// With renew_months, a passed date moves forward by whole cycles, each
// counted from expire_at and clamped to the end of shorter months.
// ok is false when the plan has no expire_at.
func (pl *Plan) Expiry(now time.Time, loc *time.Location) (date string, days int, ok bool) {
	if pl.expire.IsZero() {
		return "", 0, false
	}
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	exp := pl.expire
	for k := 1; pl.RenewMonths > 0 && exp.Before(today); k++ {
		exp = addMonths(pl.expire, k*pl.RenewMonths)
	}
	return exp.Format(time.DateOnly), int(exp.Sub(today).Hours() / 24), true
}

// addMonths keeps t's day of month, or the month's last day if it is shorter.
func addMonths(t time.Time, months int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(t.Day(), last)-1)
}
