package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shakespark/vps-probe/internal/agent/collect"
	"github.com/shakespark/vps-probe/internal/agent/config"
	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
)

type lastReport struct{ rep *pb.Report }

func (l *lastReport) Enqueue(r *pb.Report) { l.rep = r }

// fixture is a machine with one NIC, booted a day ago.
func fixture(t *testing.T) (collect.FS, func(path, content string)) {
	t.Helper()
	root := t.TempDir()
	fs := collect.FS{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys"), Etc: filepath.Join(root, "etc"), Root: root}
	write := func(path, content string) {
		t.Helper()
		path = filepath.Join(fs.Proc, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(fs.Sys, "class", "net", "eth0", "device"), 0o755)
	write("sys/kernel/random/boot_id", "b1\n")
	write("uptime", "86400.00 0.00\n")
	write("stat", "cpu  100 0 0 900 0 0 0 0 0 0\n")
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 600 kB\n")
	write("loadavg", "0.50 0.25 0.10 1/234 5678\n")
	write("net/sockstat", "TCP: inuse 27 orphan 0 tw 5 alloc 58 mem 0\nUDP: inuse 8 mem 1\n")
	write("net/dev", "Inter-|\n face |\n  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n")
	return fs, write
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("node: hk-1\nserver: 203.0.113.1:9527\ntoken: abcdefghijklmnopqrstuvwxyz0123456789\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSample(t *testing.T) {
	fs, write := fixture(t)
	var logs bytes.Buffer
	a, err := New(testConfig(t), &lastReport{}, Options{Version: "9.9.9", StateDir: t.TempDir(), FS: fs},
		slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(10 * time.Second)
	write("stat", "cpu  150 0 0 950 0 0 0 0 0 0\n")
	write("net/dev", "Inter-|\n face |\n  eth0: 21000 210 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n")
	rep := a.sample(now, now)

	if rep.Ts != now.Unix() || rep.Cpu == nil || rep.Cpu.Usage != 50 || rep.Mem.Used != 400*1024 || rep.Load.Threads != 234 || rep.Sockets.Tcp != 27 {
		t.Fatalf("report: %v", rep)
	}
	if len(rep.Net) != 1 || rep.Net[0].Iface != "eth0" || rep.Net[0].RxRate < 1900 || rep.Net[0].RxRate > 2100 || rep.Net[0].TxRate != 0 {
		t.Fatalf("net: %v", rep.Net)
	}
	// Booted inside the period (uptime one day... unless today is the 1st),
	// or not: either way the 20000 bytes since the first reading are counted.
	if len(rep.Traffic) != 1 || rep.Traffic[0].Cur.Rx < 20000 || rep.Traffic[0].Cur.End <= rep.Traffic[0].Cur.Start ||
		rep.Traffic[0].Prev.End != rep.Traffic[0].Cur.Start {
		t.Fatalf("traffic: %v", rep.Traffic)
	}
	if len(rep.Disks) != 1 || rep.Disks[0].Mount != "/" || rep.Sys == nil || rep.Sys.AgentVersion != "9.9.9" || rep.Sys.BootTime == 0 {
		t.Fatalf("disks %v, sys %v", rep.Disks, rep.Sys)
	}
	// Disks and system info are not in every report.
	now = now.Add(10 * time.Second)
	if rep := a.sample(now, now); rep.Disks != nil || rep.Sys != nil {
		t.Fatalf("second report repeats disks or sys: %v", rep)
	}

	// A source that cannot be read (lxcfs has died) is left out, the rest is
	// still reported, and the log says so once.
	os.Remove(filepath.Join(fs.Proc, "meminfo"))
	for range 3 {
		now = now.Add(10 * time.Second)
		if rep := a.sample(now, now); rep.Mem != nil || rep.Load == nil {
			t.Fatalf("report without meminfo: %v", rep)
		}
	}
	if n := strings.Count(logs.String(), "cannot read memory"); n != 1 {
		t.Fatalf("%d log lines for three failures, want 1:\n%s", n, &logs)
	}
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 600 kB\n")
	now = now.Add(10 * time.Second)
	if rep := a.sample(now, now); rep.Mem == nil || strings.Count(logs.String(), "memory can be read again") != 1 {
		t.Fatalf("after meminfo came back: %v\n%s", rep, &logs)
	}
}

// In a container whose lxcfs has died neither /proc/uptime nor /proc/stat
// can be read. The agent starts anyway, with an unknown boot time.
func TestStartsWithoutBootTime(t *testing.T) {
	fs, _ := fixture(t)
	os.Remove(filepath.Join(fs.Proc, "uptime"))
	os.Remove(filepath.Join(fs.Proc, "stat"))
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	a, err := New(testConfig(t), &lastReport{}, Options{StateDir: t.TempDir(), FS: fs}, log)
	if err != nil {
		t.Fatal(err)
	}
	if a.bootID != "b1" || !a.bootTime.IsZero() {
		t.Fatalf("bootID %q, bootTime %v; want b1 and the zero time", a.bootID, a.bootTime)
	}
	now := time.Now()
	if rep := a.sample(now, now); rep.Cpu != nil || rep.Mem == nil || rep.Sys.BootTime != 0 {
		t.Fatalf("report: %v", rep)
	}

	// Without the boot id traffic accounting cannot work: that is fatal.
	os.Remove(filepath.Join(fs.Proc, "sys", "kernel", "random", "boot_id"))
	if _, err := New(testConfig(t), &lastReport{}, Options{StateDir: t.TempDir(), FS: fs}, log); err == nil {
		t.Fatal("started without a boot id")
	}
}

func TestSecondInstanceRefused(t *testing.T) {
	fs, _ := fixture(t)
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if _, err := New(testConfig(t), &lastReport{}, Options{StateDir: dir, FS: fs}, log); err != nil {
		t.Fatal(err)
	}
	if _, err := New(testConfig(t), &lastReport{}, Options{StateDir: dir, FS: fs}, log); err == nil {
		t.Fatal("a second agent on the same state directory started")
	}
	// A dry run may run beside it.
	if _, err := New(testConfig(t), &lastReport{}, Options{StateDir: dir, FS: fs, DryRun: true}, log); err != nil {
		t.Fatal(errors.Join(errors.New("dry run refused"), err))
	}
}
