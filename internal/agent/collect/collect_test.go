package collect

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const statFixture = `cpu  1000 50 300 8000 200 10 20 70 0 0
cpu0 500 25 150 4000 100 5 10 35 0 0
intr 12345
btime 1790000000
processes 42
`

func TestParseCPU(t *testing.T) {
	c, err := parseCPU([]byte(statFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := CPUTimes{1000, 50, 300, 8000, 200, 10, 20, 70}
	if c != want {
		t.Fatalf("got %+v", c)
	}
	if _, err := parseCPU([]byte("intr 1\n")); err == nil {
		t.Fatal("accepted bad input")
	}
	if _, err := parseCPU([]byte("cpu 1 2 3\n")); err == nil {
		t.Fatal("accepted short line")
	}
}

func TestCPUUsage(t *testing.T) {
	prev := CPUTimes{User: 1000, System: 300, Idle: 8000, IOWait: 200, Steal: 100}
	// +100 total: 30 user, 10 system, 50 idle, 5 iowait, 5 steal.
	cur := CPUTimes{User: 1030, System: 310, Idle: 8050, IOWait: 205, Steal: 105}
	usage, steal, ok := CPUUsage(prev, cur)
	if !ok {
		t.Fatal("not ok")
	}
	if math.Abs(usage-45) > 1e-9 || math.Abs(steal-5) > 1e-9 {
		t.Fatalf("usage=%v steal=%v", usage, steal)
	}
	if _, _, ok := CPUUsage(cur, cur); ok {
		t.Fatal("ok with no elapsed time")
	}
	if _, _, ok := CPUUsage(cur, prev); ok {
		t.Fatal("ok with counters going backwards")
	}
}

const meminfoFixture = `MemTotal:        2000000 kB
MemFree:          300000 kB
MemAvailable:     800000 kB
Buffers:           10000 kB
SwapTotal:       1000000 kB
SwapFree:         750000 kB
HugePages_Total:       0
`

func TestParseMem(t *testing.T) {
	m, err := parseMem([]byte(meminfoFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := Mem{Total: 2000000 * 1024, Used: 1200000 * 1024, SwapTotal: 1000000 * 1024, SwapUsed: 250000 * 1024}
	if m != want {
		t.Fatalf("got %+v", m)
	}
	if _, err := parseMem([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("accepted missing MemAvailable")
	}
}

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:   12345      10    0    0    0     0          0         0    12345      10    0    0    0     0       0          0
  eth0:98765432101 123456    0    0    0     0          0         0 12345678901   65432    0    0    0     0       0          0
docker0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
`

func TestParseNetDev(t *testing.T) {
	m, err := parseNetDev([]byte(netDevFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 {
		t.Fatalf("got %d ifaces: %v", len(m), m)
	}
	if m["eth0"] != (NetCounter{RX: 98765432101, TX: 12345678901}) {
		t.Fatalf("eth0 = %+v", m["eth0"])
	}
	if m["lo"] != (NetCounter{RX: 12345, TX: 12345}) {
		t.Fatalf("lo = %+v", m["lo"])
	}
}

func fixtureFS(t *testing.T) FS {
	t.Helper()
	root := t.TempDir()
	fs := FS{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys"), Etc: filepath.Join(root, "etc")}
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(fs.Proc, "stat"), statFixture)
	write(filepath.Join(fs.Proc, "meminfo"), meminfoFixture)
	write(filepath.Join(fs.Proc, "loadavg"), "0.50 0.25 0.10 1/234 5678\n")
	write(filepath.Join(fs.Proc, "uptime"), "12345.67 45678.90\n")
	write(filepath.Join(fs.Proc, "net", "dev"), netDevFixture)
	write(filepath.Join(fs.Proc, "sys", "kernel", "random", "boot_id"), "3f2a1c9e-0000-4000-8000-000000000001\n")
	write(filepath.Join(fs.Proc, "sys", "kernel", "osrelease"), "6.1.0-test\n")
	write(filepath.Join(fs.Etc, "os-release"), "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n")

	// Physical NICs have a device link; virtual ones don't.
	net := filepath.Join(fs.Sys, "class", "net")
	for _, n := range []string{"eth0", "ens4", "lo", "docker0", "wg0", "veth123"} {
		if err := os.MkdirAll(filepath.Join(net, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"eth0", "ens4", "veth123"} {
		write(filepath.Join(net, n, "device", "uevent"), "")
	}
	return fs
}

func TestFixtureReads(t *testing.T) {
	fs := fixtureFS(t)

	if l, err := fs.ReadLoad(); err != nil || l != (Load{0.5, 0.25, 0.1}) {
		t.Fatalf("load = %+v, %v", l, err)
	}
	if bt, err := fs.BootTime(); err != nil || !bt.Equal(time.Unix(1790000000, 0)) {
		t.Fatalf("boot time = %v, %v", bt, err)
	}
	if id, err := fs.BootID(); err != nil || id != "3f2a1c9e-0000-4000-8000-000000000001" {
		t.Fatalf("boot id = %q, %v", id, err)
	}
	if up, err := fs.Uptime(); err != nil || up != 12345 {
		t.Fatalf("uptime = %v, %v", up, err)
	}
	ifaces, err := fs.DetectInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ifaces, []string{"ens4", "eth0"}) {
		t.Fatalf("ifaces = %v", ifaces)
	}
	si := fs.ReadSysInfo()
	if si.OS != "Debian GNU/Linux 12 (bookworm)" || si.Kernel != "6.1.0-test" || si.Cores < 1 {
		t.Fatalf("sysinfo = %+v", si)
	}
}

func TestDetectInterfacesNone(t *testing.T) {
	fs := FS{Sys: t.TempDir()}
	os.MkdirAll(filepath.Join(fs.Sys, "class", "net", "lo"), 0o755)
	if _, err := fs.DetectInterfaces(); err == nil {
		t.Fatal("expected error with no physical NIC")
	}
}

func TestDiskUsageRoot(t *testing.T) {
	d, err := DiskUsage("/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Used > d.Total || d.Avail > d.Total {
		t.Fatalf("implausible: %+v", d)
	}
	if _, err := DiskUsage("/definitely/not/here"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHostReads(t *testing.T) {
	// Smoke test against the real /proc on the build machine.
	if _, err := Host.ReadCPU(); err != nil {
		t.Fatal(err)
	}
	if _, err := Host.ReadMem(); err != nil {
		t.Fatal(err)
	}
	if _, err := Host.ReadNetDev(); err != nil {
		t.Fatal(err)
	}
	if _, err := Host.BootID(); err != nil {
		t.Fatal(err)
	}
}
