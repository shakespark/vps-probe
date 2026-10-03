// Package collect reads system metrics from /proc, /sys and statfs. Nothing
// here needs root.
//
// FS reads what the kernel offers at this moment. Sampler turns the running
// counters among those into the percentages and rates a report carries.
// Selector decides which network interfaces count as this machine's traffic.
package collect

import (
	"os"
	"path/filepath"
)

// FS locates the filesystems the metrics come from; tests point it at
// fixtures.
type FS struct {
	Proc string
	Sys  string
	Etc  string
	Root string // prefix of the mount points given to Disk; "" on a real machine
}

// Host is the real filesystem.
var Host = FS{Proc: "/proc", Sys: "/sys", Etc: "/etc"}

func (f FS) read(base string, parts ...string) ([]byte, error) {
	return os.ReadFile(filepath.Join(append([]string{base}, parts...)...))
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
