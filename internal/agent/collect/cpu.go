package collect

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CPUTimes holds the aggregate jiffies from the first line of /proc/stat.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

func (c CPUTimes) total() uint64 {
	// guest/guest_nice are already included in user/nice.
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// CPUTimes parses the aggregate "cpu" line of /proc/stat.
func (f FS) CPUTimes() (CPUTimes, error) {
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

// CPUPct is CPU time between two readings, in percent. SoftIRQ (mostly
// network packet processing) is part of Usage, not on top of it.
type CPUPct struct{ Usage, Steal, SoftIRQ float64 }

// CPUUsage returns the percentages between two readings. ok is false when
// no time elapsed or the counters went backwards.
func CPUUsage(prev, cur CPUTimes) (p CPUPct, ok bool) {
	pt, ct := prev.total(), cur.total()
	if ct <= pt {
		return CPUPct{}, false
	}
	dt := float64(ct - pt)
	idle := sub(cur.Idle+cur.IOWait, prev.Idle+prev.IOWait)
	share := func(d uint64) float64 { return clampPct(float64(d) / dt * 100) }
	return CPUPct{
		Usage:   clampPct((dt - float64(idle)) / dt * 100),
		Steal:   share(sub(cur.Steal, prev.Steal)),
		SoftIRQ: share(sub(cur.SoftIRQ, prev.SoftIRQ)),
	}, true
}

func clampPct(v float64) float64 {
	return max(0, min(100, v))
}
