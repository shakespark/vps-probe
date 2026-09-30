// Package collect reads system metrics from /proc, /sys and statfs. Nothing
// here needs root.
package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FS locates the proc and sys filesystems; tests point it at fixtures.
type FS struct {
	Proc string
	Sys  string
	Etc  string
}

// Host is the real filesystem.
var Host = FS{Proc: "/proc", Sys: "/sys", Etc: "/etc"}

func (f FS) read(base string, parts ...string) ([]byte, error) {
	return os.ReadFile(filepath.Join(append([]string{base}, parts...)...))
}

// ---- CPU ----

// CPUTimes holds the aggregate jiffies from the first line of /proc/stat.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

func (c CPUTimes) total() uint64 {
	// guest/guest_nice are already included in user/nice.
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// ReadCPU parses the aggregate "cpu" line of /proc/stat.
func (f FS) ReadCPU() (CPUTimes, error) {
	data, err := f.read(f.Proc, "stat")
	if err != nil {
		return CPUTimes{}, err
	}
	return parseCPU(data)
}

func parseCPU(data []byte) (CPUTimes, error) {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 9 || fields[0] != "cpu" {
		return CPUTimes{}, errors.New("collect: unexpected /proc/stat format")
	}
	var v [8]uint64
	for i := range v {
		n, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return CPUTimes{}, fmt.Errorf("collect: /proc/stat: %w", err)
		}
		v[i] = n
	}
	return CPUTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}, nil
}

// CPUUsage returns busy and steal percentages between two readings. ok is
// false when no time elapsed or the counters went backwards.
func CPUUsage(prev, cur CPUTimes) (usage, steal float64, ok bool) {
	pt, ct := prev.total(), cur.total()
	if ct <= pt {
		return 0, 0, false
	}
	dt := float64(ct - pt)
	idle := sub(cur.Idle+cur.IOWait, prev.Idle+prev.IOWait)
	usage = clampPct((dt - float64(idle)) / dt * 100)
	steal = clampPct(float64(sub(cur.Steal, prev.Steal)) / dt * 100)
	return usage, steal, true
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func clampPct(v float64) float64 {
	return max(0, min(100, v))
}

// BootTime reads btime from /proc/stat.
func (f FS) BootTime() (time.Time, error) {
	data, err := f.read(f.Proc, "stat")
	if err != nil {
		return time.Time{}, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errors.New("collect: btime not found in /proc/stat")
}

// BootID identifies the current boot; it changes on every reboot.
func (f FS) BootID() (string, error) {
	data, err := f.read(f.Proc, "sys", "kernel", "random", "boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("collect: empty boot_id")
	}
	return id, nil
}

// Uptime returns seconds since boot.
func (f FS) Uptime() (uint64, error) {
	data, err := f.read(f.Proc, "uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("collect: empty /proc/uptime")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// ---- Load ----

type Load struct{ L1, L5, L15 float64 }

func (f FS) ReadLoad() (Load, error) {
	data, err := f.read(f.Proc, "loadavg")
	if err != nil {
		return Load{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return Load{}, errors.New("collect: unexpected /proc/loadavg format")
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return Load{}, err
		}
	}
	return Load{v[0], v[1], v[2]}, nil
}

// ---- Memory ----

// Mem is in bytes. Used = Total - MemAvailable, matching what free(1) and
// most dashboards call "used".
type Mem struct {
	Total, Used, SwapTotal, SwapUsed uint64
}

func (f FS) ReadMem() (Mem, error) {
	data, err := f.read(f.Proc, "meminfo")
	if err != nil {
		return Mem{}, err
	}
	return parseMem(data)
}

func parseMem(data []byte) (Mem, error) {
	vals := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		vals[k] = n
	}
	total, ok1 := vals["MemTotal"]
	avail, ok2 := vals["MemAvailable"]
	if !ok1 || !ok2 {
		return Mem{}, errors.New("collect: MemTotal/MemAvailable missing from /proc/meminfo")
	}
	return Mem{
		Total:     total,
		Used:      sub(total, avail),
		SwapTotal: vals["SwapTotal"],
		SwapUsed:  sub(vals["SwapTotal"], vals["SwapFree"]),
	}, nil
}

// ---- Network ----

// NetCounter is a pair of cumulative kernel byte counters.
type NetCounter struct{ RX, TX uint64 }

// ReadNetDev parses /proc/net/dev.
func (f FS) ReadNetDev() (map[string]NetCounter, error) {
	data, err := f.read(f.Proc, "net", "dev")
	if err != nil {
		return nil, err
	}
	return parseNetDev(data)
}

func parseNetDev(data []byte) (map[string]NetCounter, error) {
	out := map[string]NetCounter{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// "  eth0: 123 ..." — large values can butt up against the colon.
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if name == "" || len(fields) < 16 {
			continue // header lines
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("collect: /proc/net/dev: bad counters for %s", name)
		}
		out[name] = NetCounter{RX: rx, TX: tx}
	}
	return out, sc.Err()
}

var virtualPrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "vnet", "wg", "tun", "tap",
	"tailscale", "zt", "cni", "flannel", "cali", "kube", "dummy", "ifb", "bond", "team",
}

// DetectInterfaces returns physical NICs: entries in /sys/class/net with a
// "device" link (virtio NICs have one too), minus well-known virtual names.
func (f FS) DetectInterfaces() ([]string, error) {
	dir := filepath.Join(f.Sys, "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if slices.ContainsFunc(virtualPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name, "device")); err != nil {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil, errors.New("collect: no physical network interface detected; set interfaces in config")
	}
	return out, nil
}

// ---- Disk ----

type Disk struct {
	Mount              string
	Total, Used, Avail uint64
	InodePct           float64
}

// DiskUsage matches df: used = blocks - free, avail = blocks available to
// unprivileged users.
func DiskUsage(mount string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Frsize)
	if bs == 0 {
		bs = uint64(st.Bsize)
	}
	d := Disk{
		Mount: mount,
		Total: st.Blocks * bs,
		Used:  sub(st.Blocks, st.Bfree) * bs,
		Avail: st.Bavail * bs,
	}
	if st.Files > 0 {
		d.InodePct = float64(sub(st.Files, st.Ffree)) / float64(st.Files) * 100
	}
	return d, nil
}

// ---- System info ----

type SysInfo struct {
	Hostname, OS, Kernel, Arch string
	Cores                      int
	BootTime                   time.Time
	Uptime                     uint64
}

// ReadSysInfo is best effort: missing pieces are left empty.
func (f FS) ReadSysInfo() SysInfo {
	s := SysInfo{Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	s.Hostname, _ = os.Hostname()
	if data, err := f.read(f.Proc, "sys", "kernel", "osrelease"); err == nil {
		s.Kernel = strings.TrimSpace(string(data))
	}
	s.OS = f.osPrettyName()
	s.BootTime, _ = f.BootTime()
	s.Uptime, _ = f.Uptime()
	return s
}

func (f FS) osPrettyName() string {
	data, err := f.read(f.Etc, "os-release")
	if err != nil {
		if data, err = os.ReadFile("/usr/lib/os-release"); err != nil {
			return ""
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
