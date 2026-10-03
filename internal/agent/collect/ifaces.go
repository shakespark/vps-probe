package collect

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// redetectEvery is how often an automatic selection is looked at again.
const redetectEvery = time.Minute

// Selector decides which interfaces carry this machine's billed traffic.
//
// Listed in the config, they are taken as given. Otherwise they are detected:
// the physical NICs, and of several only those carrying a default route (a
// second NIC is usually a private network that must not count against the
// quota). A machine without a physical NIC (a container) counts the
// interfaces of its default route.
//
// A choice, once made, is kept while detection cannot decide: at boot or
// during a lease renewal there may be no default route for a moment, and an
// interface selected by mistake would add its whole counter to the period.
type Selector struct {
	fs    FS
	fixed []string
	log   *slog.Logger

	state     selection
	chosen    []string
	checkedAt time.Time
}

type selection int

const (
	undecided selection = iota // never looked
	waiting                    // looked, could not decide: nothing is counted yet
	decided
)

// NewSelector returns a selector that uses fixed if it is not empty, and
// detects otherwise.
func NewSelector(fs FS, fixed []string, log *slog.Logger) *Selector {
	return &Selector{fs: fs, fixed: fixed, log: log}
}

// Interfaces returns the selection as of now, sorted. It is empty while the
// machine's interfaces cannot be told apart yet.
func (s *Selector) Interfaces(now time.Time) []string {
	if len(s.fixed) > 0 {
		return s.fixed
	}
	// Undecided, look at every sample: the network usually comes up within
	// seconds. Decided, a change of NICs is rare.
	if s.state == decided && now.Sub(s.checkedAt) < redetectEvery {
		return s.chosen
	}
	s.checkedAt = now
	found, why := s.fs.detect()
	switch {
	case len(found) == 0 && s.state == undecided:
		s.state = waiting
		s.log.Warn("traffic is not counted yet: "+why, "hint", "set interfaces in the config to choose")
	case len(found) == 0:
		// Keep waiting, or keep the previous choice.
	case s.state != decided:
		s.state, s.chosen = decided, found
		s.log.Info("counting traffic", "interfaces", found)
	case !slices.Equal(found, s.chosen):
		s.log.Warn("counted interfaces changed", "from", s.chosen, "to", found)
		s.chosen = found
	}
	return s.chosen
}

var virtualPrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "vnet", "wg", "tun", "tap",
	"tailscale", "zt", "cni", "flannel", "cali", "kube", "dummy", "ifb", "bond", "team",
}

// detect returns the interfaces to count, sorted, or none and the reason.
// Physical NICs are entries in /sys/class/net with a "device" link (virtio
// NICs have one too), minus well-known virtual names.
func (f FS) detect() (ifaces []string, why string) {
	dir := filepath.Join(f.Sys, "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err.Error()
	}
	var physical []string
	for _, e := range entries {
		name := e.Name()
		if slices.ContainsFunc(virtualPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name, "device")); err == nil {
			physical = append(physical, name)
		}
	}
	slices.Sort(physical)
	if len(physical) == 1 {
		return physical, ""
	}
	routed := f.defaultRouteInterfaces()
	slices.Sort(routed)
	switch {
	case len(physical) == 0 && len(routed) == 0:
		return nil, "no physical network interface and no default route"
	case len(physical) == 0:
		// Containers (OpenVZ's venet0, LXC's veth-backed eth0) have no
		// device behind their NIC.
		return routed, ""
	case len(routed) == 0:
		return nil, "several physical network interfaces and no default route to tell which face the internet"
	}
	facing := slices.DeleteFunc(slices.Clone(physical), func(n string) bool { return !slices.Contains(routed, n) })
	if len(facing) == 0 {
		// The default route goes through a tunnel: every NIC may carry it.
		return physical, ""
	}
	return facing, ""
}

// defaultRouteInterfaces lists the interfaces carrying an IPv4 or IPv6
// default route, from /proc/net/route and /proc/net/ipv6_route.
func (f FS) defaultRouteInterfaces() []string {
	var out []string
	add := func(name string) {
		if name != "" && name != "lo" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	// Iface Destination Gateway Flags RefCnt Use Metric Mask ...; hex, and
	// flag 0x1 is RTF_UP.
	if b, err := f.read(f.Proc, "net", "route"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Fields(line)
			if len(p) < 8 || p[1] != "00000000" || p[7] != "00000000" {
				continue
			}
			if flags, err := strconv.ParseUint(p[3], 16, 32); err == nil && flags&1 != 0 {
				add(p[0])
			}
		}
	}
	// dest prefixlen src srclen nexthop metric refcnt use flags iface
	if b, err := f.read(f.Proc, "net", "ipv6_route"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Fields(line)
			if len(p) < 10 || p[1] != "00" || strings.Trim(p[0], "0") != "" {
				continue
			}
			if flags, err := strconv.ParseUint(p[8], 16, 32); err == nil && flags&1 != 0 {
				add(p[9])
			}
		}
	}
	return out
}
