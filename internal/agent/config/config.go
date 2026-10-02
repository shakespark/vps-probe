// Package config loads the agent's YAML configuration. Everything the agent
// does is decided here, locally; nothing is ever taken from the server.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vpsprobe/internal/echo"
	"vpsprobe/internal/wire"
)

type Config struct {
	Node       string        `yaml:"node"`
	Server     Server        `yaml:"server"`
	Interval   time.Duration `yaml:"interval"`
	StateDir   string        `yaml:"state_dir"`
	Disks      []string      `yaml:"disks"`
	Interfaces []string      `yaml:"interfaces"` // empty = auto-detect
	Traffic    Traffic       `yaml:"traffic"`
	Ping       Ping          `yaml:"ping"`
}

type Server struct {
	Addr  string `yaml:"addr"` // host:port, UDP
	Token string `yaml:"token"`
}

type Traffic struct {
	Timezone  string `yaml:"timezone"`
	ResetDay  int    `yaml:"reset_day"`
	ResetTime string `yaml:"reset_time"` // HH:MM, default 00:00

	Location    *time.Location `yaml:"-"`
	ResetHour   int            `yaml:"-"`
	ResetMinute int            `yaml:"-"`
}

type Ping struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Peers    []Peer        `yaml:"peers"`
}

type Peer struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
	// icmp (default): echo to addr. For paths that only forward TCP/UDP,
	// addr is host:port and: dns sends a DNS query (e.g. to a relay port
	// forwarded to 1.1.1.1:53); echo sends a request signed with key to a
	// vps-probe-echo responder.
	Type string `yaml:"type"`
	Key  string `yaml:"key"`
}

const MinTokenLen = 32

// Load reads, defaults and validates the config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	c := &Config{
		Interval: 10 * time.Second,
		StateDir: "/var/lib/vps-probe",
		Disks:    []string{"/"},
		Traffic:  Traffic{Timezone: "Asia/Shanghai", ResetDay: 1},
		Ping:     Ping{Interval: time.Second, Timeout: 2 * time.Second},
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typos in keys are errors, not silent defaults
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

	if !wire.ValidNode(c.Node) {
		bad("node %q: must be 1-%d chars of letters, digits, '.', '_', '-'", c.Node, wire.MaxNodeLen)
	}
	if _, port, err := net.SplitHostPort(c.Server.Addr); err != nil || port == "" {
		bad("server.addr %q: want host:port", c.Server.Addr)
	}
	if len(c.Server.Token) < MinTokenLen {
		bad("server.token: must be at least %d characters (use vps-probe-server gen-token)", MinTokenLen)
	}
	if c.Interval < time.Second || c.Interval > 5*time.Minute {
		bad("interval %v: must be between 1s and 5m", c.Interval)
	}
	if !filepath.IsAbs(c.StateDir) {
		bad("state_dir %q: must be an absolute path", c.StateDir)
	}
	for _, d := range c.Disks {
		if !filepath.IsAbs(d) {
			bad("disks: %q is not an absolute path", d)
		}
	}
	for _, i := range c.Interfaces {
		if i == "" || strings.ContainsAny(i, "/ ") {
			bad("interfaces: invalid name %q", i)
		}
	}

	if c.Traffic.ResetDay < 1 || c.Traffic.ResetDay > 31 {
		bad("traffic.reset_day %d: must be 1-31", c.Traffic.ResetDay)
	}
	if c.Traffic.ResetTime != "" {
		t, err := time.Parse("15:04", c.Traffic.ResetTime)
		if err != nil {
			bad("traffic.reset_time %q: want HH:MM (24-hour)", c.Traffic.ResetTime)
		}
		c.Traffic.ResetHour, c.Traffic.ResetMinute = t.Hour(), t.Minute()
	}
	loc, err := time.LoadLocation(c.Traffic.Timezone)
	if err != nil {
		bad("traffic.timezone %q: %v", c.Traffic.Timezone, err)
	}
	c.Traffic.Location = loc

	if c.Ping.Interval < 100*time.Millisecond {
		bad("ping.interval %v: must be at least 100ms", c.Ping.Interval)
	}
	if c.Ping.Timeout <= 0 || c.Ping.Timeout >= c.Interval {
		bad("ping.timeout %v: must be positive and shorter than the report interval %v", c.Ping.Timeout, c.Interval)
	}
	seen := map[string]bool{}
	for _, p := range c.Ping.Peers {
		switch {
		case p.Name == "" || p.Addr == "":
			bad("ping.peers: name and addr are required (%+v)", p)
		case seen[p.Name]:
			bad("ping.peers: duplicate name %q", p.Name)
		case p.Type != "" && p.Type != "icmp" && p.Type != "dns" && p.Type != "echo":
			bad("ping.peers[%s].type %q: want icmp, dns or echo", p.Name, p.Type)
		case (p.Type == "dns" || p.Type == "echo") && !validHostPort(p.Addr):
			bad("ping.peers[%s].addr %q: a %s peer wants host:port, e.g. 127.0.0.1:39527", p.Name, p.Addr, p.Type)
		case p.Type == "echo" && len(p.Key) < echo.MinKeyLen:
			bad("ping.peers[%s].key: an echo peer needs the responder's key, at least %d characters", p.Name, echo.MinKeyLen)
		case p.Type != "echo" && p.Key != "":
			bad("ping.peers[%s].key: only echo peers take a key", p.Name)
		}
		seen[p.Name] = true
	}
	return errors.Join(errs...)
}

func validHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	n, perr := strconv.ParseUint(port, 10, 16)
	return err == nil && host != "" && perr == nil && n > 0
}
