// Package peer defines a latency target: what an agent pings. The same
// definition is read from agent.yml (peers) and from server.yml (a node's
// ping.extra, which is copied into that node's agent.yml).
package peer

import (
	"fmt"

	"github.com/shakespark/vps-probe/internal/echo"
	"github.com/shakespark/vps-probe/internal/netaddr"
	"github.com/shakespark/vps-probe/internal/wire"
)

// How a peer is probed.
const (
	ICMP = "icmp" // ICMP echo to Addr, a host
	DNS  = "dns"  // a DNS query over UDP to Addr, host:port
	Echo = "echo" // a signed request to a vps-probe-echo responder at Addr, host:port
)

type Peer struct {
	// Name labels the results. For another node it is that node's id, which
	// is what lines the latency matrix up.
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
	Type string `yaml:"type,omitempty"` // icmp when omitted
	Key  string `yaml:"key,omitempty"`  // echo only: the responder's key
}

// UDP reports whether the peer is probed over UDP, so that Addr has a port.
func (p Peer) UDP() bool { return p.Type == DNS || p.Type == Echo }

// Validate checks the peer and fills in the default type.
func (p *Peer) Validate() error {
	if p.Type == "" {
		p.Type = ICMP
	}
	switch {
	case !wire.ValidID(p.Name):
		return fmt.Errorf("name %q: want 1-%d chars of letters, digits, '.', '_', '-'", p.Name, wire.MaxIDLen)
	case p.Type != ICMP && !p.UDP():
		return fmt.Errorf("%s: type %q: want icmp, dns or echo", p.Name, p.Type)
	case p.Type == ICMP && !netaddr.ValidHost(p.Addr):
		return fmt.Errorf("%s: addr %q: want an IP address or host name, without port", p.Name, p.Addr)
	case p.UDP() && !netaddr.ValidHostPort(p.Addr):
		return fmt.Errorf("%s: addr %q: a %s peer wants host:port, e.g. 127.0.0.1:39527", p.Name, p.Addr, p.Type)
	case p.Type == Echo && len(p.Key) < echo.MinKeyLen:
		return fmt.Errorf("%s: key: an echo peer needs the responder's key, at least %d characters", p.Name, echo.MinKeyLen)
	case p.Type != Echo && p.Key != "":
		return fmt.Errorf("%s: key: only echo peers take a key", p.Name)
	}
	return nil
}
