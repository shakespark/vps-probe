package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/server/alert"
	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
	"github.com/shakespark/vps-probe/web"
)

type noStats struct{}

func (noStats) Stats() map[string]uint64 { return map[string]uint64{"accepted": 3} }

type noAlerts struct{}

func (noAlerts) Active() []alert.Active  { return []alert.Active{} }
func (noAlerts) Rules() []alert.RuleView { return []alert.RuleView{} }

// The current billing period of the test reports: this month.
func thisPeriod() *pb.Period {
	y, m, _ := time.Now().Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return &pb.Period{Start: start.Unix(), End: start.AddDate(0, 1, 0).Unix()}
}

func setup(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
nodes:
  - {id: hk-1, name: 香港, token: abcdefghijklmnopqrstuvwxyz0123456789, region: hk, group: 亚洲,
     traffic: {quota_gb: 1000, quota_mode: max},
     plan: {expire_at: 2099-01-01, renew_months: 12, price: "$10/年"}}
  - {id: jp-1, token: bcdefghijklmnopqrstuvwxyz0123456789a}
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
	t.Cleanup(func() { st.Close() })
	if err := st.SyncNodes([]string{"hk-1", "jp-1"}); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New()
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, st, noStats{}, noAlerts{}, "9.9.9", log).Handler(ui), st
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestReadOnlyRoutes(t *testing.T) {
	h, _ := setup(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if rec := get(t, h, m, "/api/nodes"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/nodes = %d", m, rec.Code)
		}
	}
	for path, want := range map[string]int{
		"/api/nodes":                                         200,
		"/api/sparks":                                        200,
		"/api/nodes/hk-1/metrics":                            200,
		"/api/nodes/nope/metrics":                            404,
		"/api/nodes/hk-1/metrics?from=x":                     400,
		"/api/nodes/hk-1/metrics?from=10&to=5":               400,
		"/api/nodes/hk-1/net":                                200,
		"/api/nodes/hk-1/disks":                              200,
		"/api/ping/matrix?window=10h":                        400,
		"/api/ping/availability":                             200,
		"/api/ping/availability?range=7d":                    200,
		"/api/ping/availability?range=1y":                    400,
		"/api/nodes/hk-1/ping":                               200,
		"/api/nodes/hk-1/ping/jp-1":                          200,
		"/api/nodes/nope/ping/jp-1":                          404,
		"/api/traffic":                                       200,
		"/api/traffic?periods=99":                            400,
		"/api/nodes/hk-1/traffic/daily":                      200,
		"/api/nodes/hk-1/traffic/daily?period=bad":           400,
		"/api/nodes/nope/traffic/daily":                      404,
		"/api/stats":                                         200,
		"/api/alerts":                                        200,
		"/api/alerts?from=x":                                 400,
		"/api/alerts?node=hk-1&rule=cpu&event=firing,repeat": 200,
		"/api/alerts?event=bogus":                            400,
		"/api/other":                                         404,
	} {
		rec := get(t, h, "GET", path)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d (%s)", path, rec.Code, want, strings.TrimSpace(rec.Body.String()))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing security headers", path)
		}
	}
}

func TestNodesView(t *testing.T) {
	h, st := setup(t)
	now := time.Now()
	cur := thisPeriod()
	cur.Rx, cur.Tx = 10, 20
	st.Write("hk-1", &pb.Report{Ts: now.Unix(), Cpu: &pb.CPU{Usage: 50},
		Traffic: []*pb.IfaceTraffic{{Iface: "eth0", Cur: cur}}}, netip.MustParseAddr("192.0.2.7"), now)

	var nodes []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
		Region string `json:"region"`
		Group  string `json:"group"`
		Plan   *struct {
			ExpireAt string `json:"expire_at"`
			Days     int    `json:"days"`
			Price    string `json:"price"`
		} `json:"plan"`
		Quota *struct {
			Bytes, Used int64
			Mode        string
		} `json:"quota"`
		Status *struct {
			CPU     *float64 `json:"cpu"`
			IP      string   `json:"ip"`
			Traffic *struct{ Start, End, RX, TX int64 }
		} `json:"status"`
	}
	rec := get(t, h, "GET", "/api/nodes")
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != "hk-1" || nodes[0].Name != "香港" || !nodes[0].Online ||
		nodes[0].Region != "HK" || nodes[0].Group != "亚洲" {
		t.Fatalf("nodes: %s", rec.Body)
	}
	if st := nodes[0].Status; *st.CPU != 50 || st.Traffic.TX != 20 || st.Traffic.End != cur.End || st.IP != "192.0.2.7" {
		t.Fatalf("status: %s", rec.Body)
	}
	// The quota counts the larger direction (quota_mode: max).
	if q := nodes[0].Quota; q == nil || q.Bytes != 1000<<30 || q.Used != 20 || q.Mode != "取大" {
		t.Fatalf("quota: %s", rec.Body)
	}
	if p := nodes[0].Plan; p == nil || p.ExpireAt != "2099-01-01" || p.Days < 20000 || p.Price != "$10/年" {
		t.Fatalf("plan: %s", rec.Body)
	}
	if n := nodes[1]; n.Online || n.Status != nil || n.Name != "jp-1" || n.Plan != nil || n.Quota != nil {
		t.Fatalf("silent node: %s", rec.Body)
	}

	if rec = get(t, h, "GET", "/api/stats"); !strings.Contains(rec.Body.String(), `"version":"9.9.9"`) ||
		!strings.Contains(rec.Body.String(), `"interval":10`) {
		t.Fatalf("stats: %s", rec.Body)
	}
	if rec = get(t, h, "GET", "/api/traffic"); !strings.Contains(rec.Body.String(), `"billable":20`) ||
		!strings.Contains(rec.Body.String(), `"used":20`) {
		t.Fatalf("traffic: %s", rec.Body)
	}
	rec = get(t, h, "GET", "/api/nodes/hk-1/traffic/daily")
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"period":%d`, cur.Start)) || !strings.Contains(rec.Body.String(), `"rx":10`) {
		t.Fatalf("daily: %s", rec.Body)
	}
}

// One request returns the node's links to all its peers.
func TestNodePings(t *testing.T) {
	h, st := setup(t)
	now := time.Now()
	st.Write("hk-1", &pb.Report{Ts: now.Unix() - 20, Pings: []*pb.Ping{
		{Target: "jp-1", Sent: 10, Lost: 1, Min: 1, Avg: 2, Max: 3},
		{Target: "cf", Sent: 10, Lost: 10}}}, netip.Addr{}, now)
	var got map[string]struct {
		TS   []int64               `json:"ts"`
		Cols map[string][]*float64 `json:"cols"`
	}
	rec := get(t, h, "GET", "/api/nodes/hk-1/ping")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || *got["jp-1"].Cols["avg"][0] != 2 || got["cf"].Cols["avg"][0] != nil || *got["cf"].Cols["loss_pct"][0] != 100 {
		t.Fatalf("pings: %s", rec.Body)
	}
}

func TestUI(t *testing.T) {
	h, _ := setup(t)
	rec := get(t, h, "GET", "/")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("csp = %q", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index cache-control = %q", rec.Header().Get("Cache-Control"))
	}

	req := httptest.NewRequest("GET", "/static/vendor/echarts-6.1.0.min.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" ||
		!strings.Contains(rec.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("echarts: %d %v", rec.Code, rec.Header())
	}

	req = httptest.NewRequest("GET", "/static/app.js", nil)
	req.Header.Set("If-None-Match", get(t, h, "GET", "/static/app.js").Header().Get("ETag"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidate app.js = %d", rec.Code)
	}

	for _, p := range []string{"/static/index.html", "/static/nope.js", "/index.html", "/node/x"} {
		if rec := get(t, h, "GET", p); rec.Code != 404 {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	if rec := get(t, h, "POST", "/"); rec.Code != 405 {
		t.Errorf("POST / = %d", rec.Code)
	}
}

func TestAlertsFilter(t *testing.T) {
	h, st := setup(t)
	now := time.Now().Unix()
	if err := st.SaveAlerts(store.AlertChanges{Events: []store.AlertEvent{
		{TS: now - 20, Rule: "cpu_high", Node: "hk-1", Event: "firing", Value: "95.0%", Message: "a"},
		{TS: now - 10, Rule: "offline", Node: "jp-1", Event: "firing", Message: "b"},
		{TS: now, Rule: "cpu_high", Node: "hk-1", Event: "recovered", Message: "c"},
	}}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		History []store.AlertEvent `json:"history"`
		Facets  struct {
			Nodes []string `json:"nodes"`
			Rules []string `json:"rules"`
		} `json:"facets"`
		Truncated bool `json:"truncated"`
	}
	rec := get(t, h, "GET", "/api/alerts?node=hk-1&event=firing,repeat")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	if len(got.History) != 1 || got.History[0].Message != "a" || got.History[0].Value != "95.0%" || got.Truncated {
		t.Fatalf("history %+v truncated=%v", got.History, got.Truncated)
	}
	// Facets cover the whole range, not just the filtered rows.
	if strings.Join(got.Facets.Nodes, ",") != "hk-1,jp-1" || strings.Join(got.Facets.Rules, ",") != "cpu_high,offline" {
		t.Fatalf("facets %+v", got.Facets)
	}
}
