// Package config loads the server's YAML configuration. Nodes exist only
// here: nothing registers a node at runtime.
//
// The file has one section per file of this package: the server itself
// (this file), nodes (node.go), where messages go (notify.go), and what is
// worth a message (alert.go, report.go).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shakespark/vps-probe/internal/netaddr"
	"github.com/shakespark/vps-probe/internal/server/basicauth"
)

type Config struct {
	Listen Listen `yaml:"listen"`
	// PublicAddr is how agents reach the ingest port, host:port. It is only
	// written into the agent configs this program generates.
	PublicAddr string `yaml:"public_addr"`
	DB         string `yaml:"db"`
	// Timezone is where days begin, for daily traffic, expiry dates and the
	// times in messages.
	Timezone string `yaml:"timezone"`

	Nodes  []Node    `yaml:"nodes"`
	Notify []Channel `yaml:"notify"`
	// A nil list (the key is absent) means the defaults; an empty one, none.
	Alerts  []Rule   `yaml:"alerts"`
	Reports []Report `yaml:"reports"`

	CFAccess  CFAccess  `yaml:"cf_access"`
	BasicAuth BasicAuth `yaml:"basic_auth"`
	Retention Retention `yaml:"retention"`
	Backup    Backup    `yaml:"backup"`

	Location *time.Location `yaml:"-"`
}

type Listen struct {
	Web    string `yaml:"web"`    // HTTP, meant for a reverse proxy or tunnel on loopback
	Ingest string `yaml:"ingest"` // UDP, agents report here
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

// BasicAuth, when set, makes the web UI require this user name and password
// on every request (HTTP Basic). For deployments without Cloudflare Access;
// the password travels with each request, so serve it over HTTPS only.
type BasicAuth struct {
	User         string `yaml:"user"`
	PasswordHash string `yaml:"password_hash"` // bcrypt, from `vps-probe-server hash-password`
}

func (b BasicAuth) Enabled() bool { return b.User != "" }

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

// Parse decodes, defaults and validates a config. Every problem found is
// reported, not only the first.
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
		Backup: Backup{Dir: "/var/lib/vps-probe-server/backups", Keep: 7},
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) { // EOF: an empty file, all defaults
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Alerts == nil {
		c.Alerts = DefaultRules()
	}
	if c.Reports == nil {
		c.Reports = DefaultReports()
	}
	var p problems
	c.validate(&p)
	ids := c.validateNodes(&p)
	c.validateNotify(&p)
	c.validateAlerts(&p, ids)
	c.validateReports(&p, ids)
	if err := errors.Join(p...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// problems collects everything wrong with a config.
type problems []error

func (p *problems) add(format string, a ...any) { *p = append(*p, fmt.Errorf(format, a...)) }

// validate checks the server's own settings.
func (c *Config) validate(p *problems) {
	for name, addr := range map[string]string{"listen.web": c.Listen.Web, "listen.ingest": c.Listen.Ingest} {
		if !netaddr.ValidListen(addr) {
			p.add("%s %q: want [host]:port", name, addr)
		}
	}
	if c.PublicAddr != "" && !netaddr.ValidHostPort(c.PublicAddr) {
		p.add("public_addr %q: want host:port as agents reach this server, e.g. probe.example.com:9527", c.PublicAddr)
	}
	if !filepath.IsAbs(c.DB) {
		p.add("db %q: must be an absolute path", c.DB)
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		p.add("timezone %q: %v", c.Timezone, err)
	}
	c.Location = loc

	if a := c.CFAccess; a.TeamDomain != "" || a.AUD != "" {
		team, ok := strings.CutSuffix(a.TeamDomain, ".cloudflareaccess.com")
		if !ok || team == "" || !netaddr.ValidHost(a.TeamDomain) {
			p.add("cf_access.team_domain %q: want <team>.cloudflareaccess.com (Zero Trust → Settings → Custom Pages)", a.TeamDomain)
		}
		if len(a.AUD) != 64 || strings.Trim(strings.ToLower(a.AUD), "0123456789abcdef") != "" {
			p.add("cf_access.aud: want the 64-hex-character Application Audience (AUD) tag of the Access application")
		}
	}
	if b := c.BasicAuth; b.User != "" || b.PasswordHash != "" {
		// RFC 7617: the user name cannot contain a colon.
		if b.User == "" || len(b.User) > 64 || strings.ContainsAny(b.User, ":\x00\r\n") {
			p.add("basic_auth.user: want 1-64 characters without ':'")
		}
		if err := basicauth.CheckHash(b.PasswordHash); err != nil {
			p.add("basic_auth.password_hash: want a bcrypt hash with cost >= %d, as printed by `vps-probe-server hash-password` (%v)", basicauth.MinCost, err)
		}
		if c.CFAccess.Enabled() {
			p.add("basic_auth and cf_access are both set: use one")
		}
	}

	r := c.Retention
	if time.Duration(r.Raw) < 6*time.Hour {
		p.add("retention.raw: must be at least 6h (rollups recompute the last 3h from raw)")
	}
	if r.M5 < r.Raw || r.H1 < r.M5 {
		p.add("retention: want raw <= m5 <= h1")
	}
	if c.Backup.Keep < 0 {
		p.add("backup.keep: must not be negative")
	}
	if c.Backup.Keep > 0 && !filepath.IsAbs(c.Backup.Dir) {
		p.add("backup.dir %q: must be an absolute path", c.Backup.Dir)
	}
}
