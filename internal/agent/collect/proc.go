package collect

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
)

// Threads is the 4th field's total ("running/total"): every thread on the
// host. Unlike counting /proc entries it isn't hidden by ProtectProc.
type Load struct {
	L1, L5, L15 float64
	Threads     uint32
}

func (f FS) Load() (Load, error) {
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
	l := Load{L1: v[0], L5: v[1], L15: v[2]}
	if len(fields) >= 4 {
		if _, total, ok := strings.Cut(fields[3], "/"); ok {
			if n, err := strconv.ParseUint(total, 10, 32); err == nil {
				l.Threads = uint32(n)
			}
		}
	}
	return l, nil
}

// Sockets counts sockets in use in the agent's network namespace, IPv4
// and IPv6 together.
type Sockets struct {
	TCP, UDP, TCPTimeWait uint32
}

// Sockets parses /proc/net/sockstat and sockstat6. sockstat6 is missing
// when IPv6 is disabled, which counts as no IPv6 sockets.
func (f FS) Sockets() (Sockets, error) {
	data, err := f.read(f.Proc, "net", "sockstat")
	if err != nil {
		return Sockets{}, err
	}
	v4 := parseSockstat(data)
	tcp, ok1 := v4["TCP"]["inuse"]
	udp, ok2 := v4["UDP"]["inuse"]
	tw, ok3 := v4["TCP"]["tw"]
	if !ok1 || !ok2 || !ok3 {
		return Sockets{}, errors.New("collect: unexpected /proc/net/sockstat format")
	}
	if data, err := f.read(f.Proc, "net", "sockstat6"); err == nil {
		v6 := parseSockstat(data)
		tcp += v6["TCP6"]["inuse"]
		udp += v6["UDP6"]["inuse"]
	} else if !errors.Is(err, os.ErrNotExist) {
		return Sockets{}, err
	}
	return Sockets{TCP: tcp, UDP: udp, TCPTimeWait: tw}, nil
}

// parseSockstat reads lines like "TCP: inuse 27 orphan 0 tw 0 alloc 58 mem 0"
// into protocol -> key -> value.
func parseSockstat(data []byte) map[string]map[string]uint32 {
	out := map[string]map[string]uint32{}
	for _, line := range strings.Split(string(data), "\n") {
		proto, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kv := map[string]uint32{}
		f := strings.Fields(rest)
		for i := 0; i+1 < len(f); i += 2 {
			if n, err := strconv.ParseUint(f[i+1], 10, 32); err == nil {
				kv[f[i]] = uint32(n)
			}
		}
		out[proto] = kv
	}
	return out
}

// Mem is in bytes. Used = Total - MemAvailable, matching what free(1) and
// most dashboards call "used".
type Mem struct {
	Total, Used, SwapTotal, SwapUsed uint64
}

func (f FS) Mem() (Mem, error) {
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
