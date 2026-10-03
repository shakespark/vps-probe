package agent

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shakespark/vps-probe/internal/agent/collect"
	"github.com/shakespark/vps-probe/internal/agent/config"
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

// In a container whose lxcfs has died /proc/stat cannot be read. The agent
// starts anyway, with an unknown boot time, and says so once per collector
// rather than at every sample.
func TestStartsWithoutBootTime(t *testing.T) {
	fs := collect.FS{Proc: t.TempDir()}
	os.MkdirAll(filepath.Join(fs.Proc, "sys", "kernel", "random"), 0o755)
	os.WriteFile(filepath.Join(fs.Proc, "sys", "kernel", "random", "boot_id"), []byte("b1\n"), 0o644)
	var logs bytes.Buffer
	a := &Agent{fs: fs, log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := a.readBoot(); err != nil {
		t.Fatal(err)
	}
	if a.bootID != "b1" || !a.bootTime.IsZero() {
		t.Fatalf("bootID %q, bootTime %v; want b1 and the zero time", a.bootID, a.bootTime)
	}

	stat := filepath.Join(fs.Proc, "stat")
	read := func() bool {
		_, err := a.fs.ReadCPU()
		return a.collected("cpu", err)
	}
	logs.Reset()
	if read() || read() || read() {
		t.Fatal("reading a missing /proc/stat succeeded")
	}
	if n := strings.Count(logs.String(), "read cpu"); n != 1 {
		t.Fatalf("%d log lines for three failures, want 1:\n%s", n, &logs)
	}
	os.WriteFile(stat, []byte("cpu  1 2 3 4 5 6 7 8 9 10\nbtime 1700000000\n"), 0o644)
	if !read() || !read() {
		t.Fatal("reading /proc/stat failed")
	}
	if n := strings.Count(logs.String(), "readable again"); n != 1 {
		t.Fatalf("%d recovery lines, want 1:\n%s", n, &logs)
	}
	os.Remove(stat)
	read()
	if n := strings.Count(logs.String(), "not reported until"); n != 2 {
		t.Fatalf("a second outage logged %d failure lines in total, want 2:\n%s", n, &logs)
	}

	// Without the boot id traffic accounting cannot work: that is fatal.
	if err := (&Agent{fs: collect.FS{Proc: t.TempDir()}, log: a.log}).readBoot(); err == nil {
		t.Fatal("readBoot without a boot id succeeded")
	}
}
