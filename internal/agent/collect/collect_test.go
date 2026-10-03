package collect

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	prev := CPUTimes{User: 1000, System: 300, Idle: 8000, IOWait: 200, SoftIRQ: 50, Steal: 100}
	// +100 total: 25 user, 10 system, 50 idle, 5 iowait, 5 softirq, 5 steal.
	cur := CPUTimes{User: 1025, System: 310, Idle: 8050, IOWait: 205, SoftIRQ: 55, Steal: 105}
	p, ok := CPUUsage(prev, cur)
	if !ok {
		t.Fatal("not ok")
	}
	if math.Abs(p.Usage-45) > 1e-9 || math.Abs(p.Steal-5) > 1e-9 || math.Abs(p.SoftIRQ-5) > 1e-9 {
		t.Fatalf("got %+v", p)
	}
	if _, ok := CPUUsage(cur, cur); ok {
		t.Fatal("ok with no elapsed time")
	}
	if _, ok := CPUUsage(cur, prev); ok {
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
	if m["eth0"] != (NetCounter{RX: 98765432101, TX: 12345678901, RXPkts: 123456, TXPkts: 65432}) {
		t.Fatalf("eth0 = %+v", m["eth0"])
	}
	if m["lo"] != (NetCounter{RX: 12345, TX: 12345, RXPkts: 10, TXPkts: 10}) {
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
	write(filepath.Join(fs.Proc, "net", "sockstat"), "sockets: used 418\nTCP: inuse 27 orphan 0 tw 5 alloc 58 mem 0\n"+
		"UDP: inuse 8 mem 1023\nUDPLITE: inuse 0\nRAW: inuse 0\nFRAG: inuse 0 memory 0\n")
	write(filepath.Join(fs.Proc, "net", "sockstat6"), "TCP6: inuse 3\nUDP6: inuse 1\nUDPLITE6: inuse 0\nRAW6: inuse 0\nFRAG6: inuse 0 memory 0\n")
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

	if l, err := fs.ReadLoad(); err != nil || l != (Load{0.5, 0.25, 0.1, 234}) {
		t.Fatalf("load = %+v, %v", l, err)
	}
	if k, err := fs.ReadSockets(); err != nil || k != (Sockets{TCP: 30, UDP: 9, TCPTimeWait: 5}) {
		t.Fatalf("sockets = %+v, %v", k, err)
	}
	// IPv6 disabled: no sockstat6.
	os.Remove(filepath.Join(fs.Proc, "net", "sockstat6"))
	if k, err := fs.ReadSockets(); err != nil || k != (Sockets{TCP: 27, UDP: 8, TCPTimeWait: 5}) {
		t.Fatalf("sockets without IPv6 = %+v, %v", k, err)
	}
	os.WriteFile(filepath.Join(fs.Proc, "net", "sockstat"), []byte("sockets: used 1\n"), 0o644)
	if _, err := fs.ReadSockets(); err == nil {
		t.Fatal("truncated sockstat accepted")
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
	// Two physical NICs (eth0, ens4) and no route table: cannot choose yet.
	if ifaces, err := fs.DetectInterfaces(); !errors.Is(err, ErrNoDefaultRoute) {
		t.Fatalf("ifaces = %v, %v", ifaces, err)
	}
	si := fs.ReadSysInfo()
	if si.OS != "Debian GNU/Linux 12 (bookworm)" || si.Kernel != "6.1.0-test" || si.Cores < 1 {
		t.Fatalf("sysinfo = %+v", si)
	}
}

func TestDetectInterfacesNone(t *testing.T) {
	fs := FS{Sys: t.TempDir(), Proc: t.TempDir()}
	os.MkdirAll(filepath.Join(fs.Sys, "class", "net", "lo"), 0o755)
	if _, err := fs.DetectInterfaces(); err == nil {
		t.Fatal("expected error with no physical NIC")
	}
}

// Of several physical NICs only those with a default route are counted; one
// physical NIC is counted whatever the routes say.
func TestDetectInterfacesMultiNIC(t *testing.T) {
	const hdr = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	v4 := func(iface string) string { return iface + "\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" }
	v6 := func(iface string) string {
		return "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     " + iface + "\n"
	}
	lan := "eth1\t0014A8C0\t00000000\t0001\t0\t0\t0\t00FCFFFF\t0\t0\t0\n"
	for name, c := range map[string]struct {
		phys          []string
		route, route6 string
		want          string
		noRoute       bool
	}{
		"public eth0, private eth1":      {phys: []string{"eth0", "eth1"}, route: hdr + v4("eth0") + lan, route6: v6("eth0"), want: "eth0"},
		"IPv4 on eth0, IPv6 on eth1":     {phys: []string{"eth0", "eth1"}, route: hdr + v4("eth0"), route6: v6("eth1"), want: "eth0,eth1"},
		"two uplinks":                    {phys: []string{"eth0", "eth1", "eth2"}, route: hdr + v4("eth0") + v4("eth2"), want: "eth0,eth2"},
		"default route through a tunnel": {phys: []string{"eth0", "eth1"}, route: hdr + v4("wg0") + lan, want: "eth0,eth1"},
		"no default route":               {phys: []string{"eth0", "eth1"}, route: hdr + lan, noRoute: true},
		"no route table":                 {phys: []string{"eth0", "eth1"}, noRoute: true},
		"one NIC, tunnel default":        {phys: []string{"eth0"}, route: hdr + v4("wg0"), want: "eth0"},
		"one NIC, no default route":      {phys: []string{"eth0"}, route: hdr, want: "eth0"},
	} {
		fs := FS{Sys: t.TempDir(), Proc: t.TempDir()}
		for _, n := range append([]string{"lo", "wg0"}, c.phys...) {
			os.MkdirAll(filepath.Join(fs.Sys, "class", "net", n), 0o755)
		}
		for _, n := range c.phys {
			os.MkdirAll(filepath.Join(fs.Sys, "class", "net", n, "device"), 0o755)
		}
		os.MkdirAll(filepath.Join(fs.Proc, "net"), 0o755)
		if c.route != "" {
			os.WriteFile(filepath.Join(fs.Proc, "net", "route"), []byte(c.route), 0o644)
		}
		if c.route6 != "" {
			os.WriteFile(filepath.Join(fs.Proc, "net", "ipv6_route"), []byte(c.route6), 0o644)
		}
		got, err := fs.DetectInterfaces()
		if c.noRoute {
			if !errors.Is(err, ErrNoDefaultRoute) {
				t.Errorf("%s: got %v, %v; want ErrNoDefaultRoute", name, got, err)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != c.want {
			t.Errorf("%s: got %v, %v; want %q", name, got, err, c.want)
		}
	}
}

// Containers have no device behind their NIC: the default route's interface
// is counted instead.
func TestDetectInterfacesContainer(t *testing.T) {
	const hdr = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	for name, c := range map[string]struct {
		route, route6 string
		want          string
	}{
		"OpenVZ venet0": {
			route: hdr + "venet0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n",
			want:  "venet0",
		},
		"LXC eth0, with a LAN route": {
			route: hdr + "eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
				"eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			want: "eth0",
		},
		"IPv6 only": {
			route: hdr,
			route6: "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth1\n" +
				"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n" +
				"fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth2\n",
			want: "eth1",
		},
		"both families, two interfaces": {
			route:  hdr + "venet0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n",
			route6: "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001     eth0\n",
			want:   "eth0,venet0",
		},
		"default route that is down": {
			route: hdr + "eth0\t00000000\t0100A8C0\t0002\t0\t0\t0\t00000000\t0\t0\t0\n",
			want:  "",
		},
		"no default route": {route: hdr, want: ""},
	} {
		fs := FS{Sys: t.TempDir(), Proc: t.TempDir()}
		for _, n := range []string{"lo", "venet0", "eth0", "eth1"} { // none has a device link
			os.MkdirAll(filepath.Join(fs.Sys, "class", "net", n), 0o755)
		}
		os.MkdirAll(filepath.Join(fs.Proc, "net"), 0o755)
		os.WriteFile(filepath.Join(fs.Proc, "net", "route"), []byte(c.route), 0o644)
		if c.route6 != "" {
			os.WriteFile(filepath.Join(fs.Proc, "net", "ipv6_route"), []byte(c.route6), 0o644)
		}
		got, err := fs.DetectInterfaces()
		if strings.Join(got, ",") != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%s: got %v, %v; want %q", name, got, err, c.want)
		}
	}
}

// A machine with a real NIC never consults the routes.
func TestDetectInterfacesPrefersPhysical(t *testing.T) {
	fs := FS{Sys: t.TempDir(), Proc: t.TempDir()}
	os.MkdirAll(filepath.Join(fs.Sys, "class", "net", "ens3", "device"), 0o755)
	os.MkdirAll(filepath.Join(fs.Sys, "class", "net", "tun0"), 0o755)
	os.MkdirAll(filepath.Join(fs.Proc, "net"), 0o755)
	os.WriteFile(filepath.Join(fs.Proc, "net", "route"), []byte("tun0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"), 0o644)
	got, err := fs.DetectInterfaces()
	if err != nil || strings.Join(got, ",") != "ens3" {
		t.Fatalf("got %v, %v", got, err)
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
	if k, err := Host.ReadSockets(); err != nil || k.TCP+k.UDP == 0 {
		t.Fatalf("sockets = %+v, %v", k, err)
	}
	if l, err := Host.ReadLoad(); err != nil || l.Threads == 0 {
		t.Fatalf("load = %+v, %v", l, err)
	}
}

// In an LXC container btime is the host's boot and the uptime the
// container's: the uptime wins, btime is the fallback.
func TestSystemStart(t *testing.T) {
	fs := FS{Proc: t.TempDir()}
	now := time.Unix(1800000000, 500)
	if _, err := fs.SystemStart(now); err == nil {
		t.Fatal("start time without /proc/uptime and /proc/stat")
	}
	os.WriteFile(filepath.Join(fs.Proc, "stat"), []byte("cpu  1 2 3 4 5 6 7 8 9 10\nbtime 1700000000\n"), 0o644)
	if st, err := fs.SystemStart(now); err != nil || !st.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("from btime = %v, %v", st, err)
	}
	os.WriteFile(filepath.Join(fs.Proc, "uptime"), []byte("728.25 728.25\n"), 0o644)
	if st, err := fs.SystemStart(now); err != nil || !st.Equal(time.Unix(1800000000-728, 0)) {
		t.Fatalf("from uptime = %v, %v", st, err)
	}
}
