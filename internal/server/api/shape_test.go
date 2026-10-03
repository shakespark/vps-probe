package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/server/alert"
	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/notify"
	"github.com/shakespark/vps-probe/internal/server/store"
)

// The demo site (docs/DESIGN.md §11) answers the API from a script, which
// goes stale silently when a response gains a field. This test pins the
// structure of every response - names and types, no values - in
// web/demo/api-shape.json; web/demo/check.js holds the demo to the same file.
// After changing a response: go test ./internal/server/api -update-shape,
// then make demo-check.
var updateShape = flag.Bool("update-shape", false, "rewrite web/demo/api-shape.json")

const shapeFile = "../../../web/demo/api-shape.json"

// Paths exist in both the test fixture and the demo. dynamic lists the
// objects keyed by data (interface, mount, counter name) rather than by
// field name; "" is the response itself.
var shapeEndpoints = []struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Dynamic []string `json:"dynamic,omitempty"`
	Shape   any      `json:"shape"`
}{
	{Name: "nodes", Path: "/api/nodes"},
	{Name: "sparks", Path: "/api/sparks", Dynamic: []string{"nodes"}},
	{Name: "metrics", Path: "/api/nodes/hk-1/metrics"},
	{Name: "net", Path: "/api/nodes/hk-1/net", Dynamic: []string{""}},
	{Name: "disks", Path: "/api/nodes/hk-1/disks", Dynamic: []string{""}},
	{Name: "pings", Path: "/api/nodes/hk-1/ping", Dynamic: []string{""}},
	{Name: "ping", Path: "/api/nodes/hk-1/ping/tyo-1"},
	{Name: "matrix", Path: "/api/ping/matrix?window=1h"},
	{Name: "availability", Path: "/api/ping/availability?range=24h"},
	{Name: "traffic", Path: "/api/traffic"},
	{Name: "daily", Path: "/api/nodes/hk-1/traffic/daily"},
	{Name: "stats", Path: "/api/stats", Dynamic: []string{"ingest"}},
	{Name: "alerts", Path: "/api/alerts"},
}

// The real evaluator describes the rules; one alert is made to be active.
type oneActive struct{ *alert.Evaluator }

func (oneActive) Active() []alert.Active {
	return []alert.Active{{Rule: "cpu_high", Node: "hk-1", Firing: true, Since: 1, Value: "99.0%"}}
}

// shapeOf reduces a decoded JSON value to its structure: "string", "number",
// "bool", "null", {"[]": element} or {"{}": {field: shape}}; an object on a
// dynamic path becomes {"{*}": value}. Elements are merged, so a field any
// element has is in the shape.
func shapeOf(v any, path string, dynamic []string) any {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		var el any = "null"
		for _, x := range v {
			el = mergeShape(el, shapeOf(x, path, dynamic))
		}
		return map[string]any{"[]": el}
	case map[string]any:
		if slices.Contains(dynamic, path) {
			var el any = "null"
			for _, x := range v {
				el = mergeShape(el, shapeOf(x, path+".*", dynamic))
			}
			return map[string]any{"{*}": el}
		}
		fields := map[string]any{}
		for k, x := range v {
			fields[k] = shapeOf(x, strings.TrimPrefix(path+"."+k, "."), dynamic)
		}
		return map[string]any{"{}": fields}
	}
	panic(fmt.Sprintf("unexpected JSON value %T", v))
}

// mergeShape: null yields to anything, containers merge, and two different
// types make "any".
func mergeShape(a, b any) any {
	if a == "null" {
		return b
	}
	if b == "null" {
		return a
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		if a == b {
			return a
		}
		return "any"
	}
	for _, kind := range []string{"[]", "{*}"} {
		if x, ok := am[kind]; ok {
			if y, ok := bm[kind]; ok {
				return map[string]any{kind: mergeShape(x, y)}
			}
			return "any"
		}
	}
	af, aok := am["{}"].(map[string]any)
	bf, bok := bm["{}"].(map[string]any)
	if !aok || !bok {
		return "any"
	}
	out := map[string]any{}
	for k, x := range af {
		out[k] = x
	}
	for k, y := range bf {
		if x, ok := out[k]; ok {
			out[k] = mergeShape(x, y)
		} else {
			out[k] = y
		}
	}
	return map[string]any{"{}": out}
}

// emptyArrays lists the arrays whose element shape is unknown: the fixture
// must put data in every one of them, or the file checks nothing there.
func emptyArrays(s any, path string) []string {
	m, ok := s.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, kind := range []string{"[]", "{*}"} {
		if el, ok := m[kind]; ok {
			if el == "null" {
				return []string{path + kind}
			}
			return emptyArrays(el, path+kind)
		}
	}
	for k, x := range m["{}"].(map[string]any) {
		out = append(out, emptyArrays(x, path+"."+k)...)
	}
	return out
}

func TestAPIShape(t *testing.T) {
	cfg, err := config.Parse([]byte(`
nodes:
  - {id: hk-1, name: 香港, token: abcdefghijklmnopqrstuvwxyz0123456789, region: hk, group: 亚洲,
     traffic: {quota_gb: 1000}, plan: {expire_at: 2099-01-01, renew_months: 12, price: "$10/年"}, ping: {addr: 192.0.2.1}}
  - {id: tyo-1, token: bcdefghijklmnopqrstuvwxyz0123456789a, ping: {addr: 192.0.2.2}}
notify:
  - {type: webhook, name: hook, url: "https://example.com/hook"}
alerts:
  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, for: 5m, nodes: [hk-1]}
  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4, for: 2m, exclude: [tyo-1]}
reports:
  - {type: traffic_quota, levels: [80, 100]}
  - {type: weekly, at: "Mon 10:00"}
`))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"), store.Options{Location: cfg.Location,
		Retention: store.Retention{Raw: 48 * time.Hour, M5: 720 * time.Hour, H1: 9600 * time.Hour}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SyncNodes([]string{"hk-1", "tyo-1"}); err != nil {
		t.Fatal(err)
	}

	// Every field an agent can report, old enough to be in the 5m rollups
	// (availability reads those) and once more just now.
	now := time.Now().Truncate(10 * time.Second)
	// One period for both reports, whatever the date: around the start of a
	// month the earlier report must not open a second, empty-looking one.
	y, m, _ := now.In(cfg.Location).Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, cfg.Location)
	period := func(rx, tx uint64) *pb.Period {
		return &pb.Period{Start: start.Unix(), End: start.AddDate(0, 1, 0).Unix(), Rx: rx, Tx: tx}
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now} {
		rep := &pb.Report{
			Ts:      at.Unix(),
			Sys:     &pb.SysInfo{Hostname: "h", Os: "Debian", Kernel: "6.1", Arch: "amd64", Cores: 2, BootTime: 1, AgentVersion: "9.9.9"},
			Cpu:     &pb.CPU{Usage: 50, Steal: 1, Softirq: 2},
			Load:    &pb.Load{L1: 1, L5: 1, L15: 1, Threads: 100},
			Mem:     &pb.Mem{Total: 100, Used: 50, SwapTotal: 10, SwapUsed: 1},
			Disks:   []*pb.Disk{{Mount: "/", Total: 100, Used: 40, Avail: 55, InodePct: 3}},
			Net:     []*pb.NetRate{{Iface: "eth0", RxRate: 10, TxRate: 20, RxPps: 1, TxPps: 2}},
			Traffic: []*pb.IfaceTraffic{{Iface: "eth0", Cur: period(10, 20)}},
			Pings:   []*pb.Ping{{Target: "tyo-1", Addr: "192.0.2.2", Sent: 10, Lost: 1, Min: 1, Avg: 2, Max: 3, Jitter: 0.5}},
			Sockets: &pb.Sockets{Tcp: 5, Udp: 3, TcpTw: 1},
		}
		if err := st.Write("hk-1", rep, netip.MustParseAddr("192.0.2.7"), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Rollup(now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAlerts(store.AlertChanges{Events: []store.AlertEvent{
		{TS: now.Unix(), Rule: "cpu_high", Node: "hk-1", Target: "x", Event: "firing", Value: "99.0%", Message: "m"},
	}}); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, st, noStats{}, oneActive{alert.New(cfg, st, notify.Log{Log: log}, log)}, "9.9.9", log).Handler(nil)

	for i := range shapeEndpoints {
		e := &shapeEndpoints[i]
		rec := get(t, h, "GET", e.Path)
		var v any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); rec.Code != 200 || err != nil {
			t.Fatalf("GET %s = %d %v: %s", e.Path, rec.Code, err, rec.Body)
		}
		e.Shape = shapeOf(v, "", e.Dynamic)
		if empty := emptyArrays(e.Shape, e.Name); len(empty) > 0 {
			t.Errorf("%s: the fixture leaves these empty, so their shape is unchecked: %v", e.Name, empty)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(shapeEndpoints); err != nil {
		t.Fatal(err)
	}
	if *updateShape {
		if err := os.WriteFile(shapeFile, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(shapeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("API responses no longer match %s. If the change is intended, run\n"+
			"  go test ./internal/server/api -update-shape\n"+
			"and then update web/demo/demo.js until `make demo-check` passes.", shapeFile)
	}
}

// The demo site sends the same security headers as the server.
func TestDemoHeaders(t *testing.T) {
	b, err := os.ReadFile("../../../web/demo/_headers")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Content-Security-Policy: "+csp+"\n") {
		t.Fatalf("web/demo/_headers does not carry the server's CSP:\n%s", csp)
	}
}
