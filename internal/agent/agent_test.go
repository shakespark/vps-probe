package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"vpsprobe/internal/agent/collect"
	"vpsprobe/internal/agent/config"
)

// A machine with a public and a private NIC: while there is no default
// route the private one must never be selected, because a newly selected
// interface is counted since boot.
func TestRefreshIfacesWaitsForDefaultRoute(t *testing.T) {
	fs := collect.FS{Sys: t.TempDir(), Proc: t.TempDir()}
	for _, n := range []string{"eth0", "eth1"} {
		os.MkdirAll(filepath.Join(fs.Sys, "class", "net", n, "device"), 0o755)
	}
	os.MkdirAll(filepath.Join(fs.Proc, "net"), 0o755)
	route := filepath.Join(fs.Proc, "net", "route")
	const hdr = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	up := func(on bool) {
		s := hdr
		if on {
			s += "eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
		}
		os.WriteFile(route, []byte(s), 0o644)
	}
	a := &Agent{cfg: &config.Config{}, fs: fs, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Now()
	step := func(d time.Duration, want ...string) {
		t.Helper()
		now = now.Add(d)
		if err := a.refreshIfaces(now); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(a.ifaces, want) {
			t.Fatalf("ifaces = %v, want %v", a.ifaces, want)
		}
	}

	up(false)
	step(0)                // boot, network not up: nothing yet
	step(10 * time.Second) // still nothing, and it looked again
	up(true)
	step(10*time.Second, "eth0") // picked up at the next sample, not a minute later
	up(false)
	step(2*time.Minute, "eth0") // the route goes away: keep the choice
	up(true)
	step(2*time.Minute, "eth0")
}
