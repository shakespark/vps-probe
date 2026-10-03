// Package config loads the agent's YAML configuration. Everything the agent
// does is decided here, locally; nothing is ever taken from the server.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shakespark/vps-probe/internal/agent/traffic"
	"github.com/shakespark/vps-probe/internal/netaddr"
	"github.com/shakespark/vps-probe/internal/peer"
	"github.com/shakespark/vps-probe/internal/wire"
)

type Config struct {
	Node       string      `yaml:"node"`   // this node's id in server.yml
	Server     string      `yaml:"server"` // host:port, UDP
	Token      string      `yaml:"token"`
	Disks      []string    `yaml:"disks"`
	Interfaces []string    `yaml:"interfaces"` // empty = auto-detect
	Traffic    Traffic     `yaml:"traffic"`
	Peers      []peer.Peer `yaml:"peers"`
}

// Traffic says when this node's billing period begins.
type Traffic struct {
	Timezone  string `yaml:"timezone"`
	ResetDay  int    `yaml:"reset_day"`
	ResetTime string `yaml:"reset_time"` // HH:MM

	Location *time.Location `yaml:"-"`
	Reset    traffic.Reset  `yaml:"-"`
}

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
		Disks:   []string{"/"},
		Traffic: Traffic{Timezone: "Asia/Shanghai", ResetDay: 1, ResetTime: "00:00"},
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

	if !wire.ValidID(c.Node) {
		bad("node %q: must be 1-%d chars of letters, digits, '.', '_', '-'", c.Node, wire.MaxIDLen)
	}
	if !netaddr.ValidHostPort(c.Server) {
		bad("server %q: want host:port", c.Server)
	}
	if len(c.Token) < wire.MinTokenLen {
		bad("token: must be at least %d characters (use vps-probe-server gen-token)", wire.MinTokenLen)
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
	if err := c.Traffic.validate(); err != nil {
		errs = append(errs, err)
	}
	seen := map[string]bool{}
	for i := range c.Peers {
		p := &c.Peers[i]
		if err := p.Validate(); err != nil {
			bad("peers: %w", err)
		} else if seen[p.Name] {
			bad("peers: duplicate name %q", p.Name)
		}
		seen[p.Name] = true
	}
	return errors.Join(errs...)
}

func (t *Traffic) validate() error {
	var errs []error
	if t.ResetDay < 1 || t.ResetDay > 31 {
		errs = append(errs, fmt.Errorf("traffic.reset_day %d: must be 1-31", t.ResetDay))
	}
	at, err := time.Parse("15:04", t.ResetTime)
	if err != nil {
		errs = append(errs, fmt.Errorf("traffic.reset_time %q: want HH:MM (24-hour)", t.ResetTime))
	}
	if t.Location, err = time.LoadLocation(t.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("traffic.timezone %q: %v", t.Timezone, err))
	}
	t.Reset = traffic.Reset{Day: t.ResetDay, Hour: at.Hour(), Minute: at.Minute()}
	return errors.Join(errs...)
}
