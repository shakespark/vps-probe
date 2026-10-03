package collect

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// NetCounter holds an interface's cumulative kernel counters.
type NetCounter struct{ RX, TX, RXPkts, TXPkts uint64 }

// NetCounters parses /proc/net/dev.
func (f FS) NetCounters() (map[string]NetCounter, error) {
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
		var v [4]uint64
		for i, f := range []int{0, 8, 1, 9} { // rx/tx bytes, rx/tx packets
			n, err := strconv.ParseUint(fields[f], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("collect: /proc/net/dev: bad counters for %s", name)
			}
			v[i] = n
		}
		out[name] = NetCounter{RX: v[0], TX: v[1], RXPkts: v[2], TXPkts: v[3]}
	}
	return out, sc.Err()
}
