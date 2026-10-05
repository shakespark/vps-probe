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
	// EchoTCP is Echo over one long-lived TCP connection, for a tunnel
	// that carries no UDP.
	EchoTCP = "echo-tcp"
)

type Peer struct {
	// Name labels the results. For another node it is that node's id, which
	// is what lines the latency matrix up.
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
	Type string `yaml:"type,omitempty"` // icmp when omitted
	Key  string `yaml:"key,omitempty"`  // echo and echo-tcp only: the responder's key
}

// UDP reports whether the peer is probed over UDP.
func (p Peer) UDP() bool { return p.Type == DNS || p.Type == Echo }

// HasPort reports whether Addr is host:port rather than a host.
func (p Peer) HasPort() bool { return p.UDP() || p.Type == EchoTCP }

// Signed reports whether the peer is a vps-probe-echo responder, so that it
// needs Key.
func (p Peer) Signed() bool { return p.Type == Echo || p.Type == EchoTCP }

// Validate checks the peer and fills in the default type.
func (p *Peer) Validate() error {
	if p.Type == "" {
		p.Type = ICMP
	}
	switch {
	case !wire.ValidID(p.Name):
		return fmt.Errorf("name %q: want 1-%d chars of letters, digits, '.', '_', '-'", p.Name, wire.MaxIDLen)
	case p.Type != ICMP && !p.HasPort():
		return fmt.Errorf("%s: type %q: want icmp, dns, echo or echo-tcp", p.Name, p.Type)
	case p.Type == ICMP && !netaddr.ValidHost(p.Addr):
		return fmt.Errorf("%s: addr %q: want an IP address or host name, without port", p.Name, p.Addr)
	case p.HasPort() && !netaddr.ValidHostPort(p.Addr):
		return fmt.Errorf("%s: addr %q: a %s peer wants host:port, e.g. 127.0.0.1:39527", p.Name, p.Addr, p.Type)
	case p.Signed() && len(p.Key) < echo.MinKeyLen:
		return fmt.Errorf("%s: key: an %s peer needs the responder's key, at least %d characters", p.Name, p.Type, echo.MinKeyLen)
	case !p.Signed() && p.Key != "":
		return fmt.Errorf("%s: key: only echo and echo-tcp peers take a key", p.Name)
	}
	return nil
}
