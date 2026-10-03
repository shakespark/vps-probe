package collect

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// bootTime reads btime from /proc/stat.
func (f FS) bootTime() (time.Time, error) {
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

// SystemStart is when this system started: now minus /proc/uptime, or btime
// when the uptime cannot be read. On a real or fully virtualized machine the
// two agree. In an LXC container lxcfs reports the container's uptime and
// leaves btime as the host's boot, and the interface counters start with the
// container, so the uptime is the one that says since when they count.
func (f FS) SystemStart(now time.Time) (time.Time, error) {
	if up, err := f.uptime(); err == nil {
		return now.Truncate(time.Second).Add(-time.Duration(up) * time.Second), nil
	}
	return f.bootTime()
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

// uptime returns seconds since boot.
func (f FS) uptime() (uint64, error) {
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

type SysInfo struct {
	Hostname, OS, Kernel, Arch string
	Cores                      int
	BootTime                   time.Time // zero when unknown
}

// SysInfo is best effort: missing pieces are left empty.
func (f FS) SysInfo() SysInfo {
	s := SysInfo{Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	s.Hostname, _ = os.Hostname()
	if data, err := f.read(f.Proc, "sys", "kernel", "osrelease"); err == nil {
		s.Kernel = strings.TrimSpace(string(data))
	}
	s.OS = f.osPrettyName()
	s.BootTime, _ = f.SystemStart(time.Now())
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
