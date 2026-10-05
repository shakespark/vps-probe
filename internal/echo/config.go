package echo

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/shakespark/vps-probe/internal/netaddr"
)

// Config is the responder's echo.yml.
type Config struct {
	Listen string   `yaml:"listen"`
	TCP    bool     `yaml:"tcp"` // also answer on TCP, same address
	Key    string   `yaml:"key"`
	Allow  []string `yaml:"allow"`   // source IPs or CIDRs; empty = any
	MaxPPS int      `yaml:"max_pps"` // 0 = the default, 1000

	Prefixes []netip.Prefix `yaml:"-"` // Allow, parsed
}

// LoadConfig reads, defaults and validates the config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: ":39527"}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var errs []error
	if !netaddr.ValidListen(c.Listen) {
		errs = append(errs, fmt.Errorf("listen %q: want [host]:port", c.Listen))
	}
	if len(c.Key) < MinKeyLen {
		errs = append(errs, fmt.Errorf("key: must be at least %d characters (use vps-probe-server gen-token)", MinKeyLen))
	}
	for _, a := range c.Allow {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				errs = append(errs, fmt.Errorf("allow: %q is not an IP or CIDR", a))
				continue
			}
			p = netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen())
		}
		c.Prefixes = append(c.Prefixes, p.Masked())
	}
	if c.MaxPPS < 0 {
		errs = append(errs, errors.New("max_pps: must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}
