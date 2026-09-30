package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "vpsprobe/internal/proto/probev1"
	"vpsprobe/internal/server/alert"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/server/store"
	"vpsprobe/web"
)

type noStats struct{}

func (noStats) Stats() map[string]uint64 { return map[string]uint64{"accepted": 3} }

type noAlerts struct{}

func (noAlerts) Active() []alert.Active { return []alert.Active{} }

func setup(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
nodes:
  - {id: hk-1, name: 香港, token: abcdefghijklmnopqrstuvwxyz0123456789, traffic_quota_gb: 1000}
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
	return New(cfg, st, noStats{}, noAlerts{}, log).Handler(ui), st
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
		"/api/nodes":                           200,
		"/api/nodes/hk-1/metrics":              200,
		"/api/nodes/nope/metrics":              404,
		"/api/nodes/hk-1/metrics?from=x":       400,
		"/api/nodes/hk-1/metrics?from=10&to=5": 400,
		"/api/nodes/hk-1/net":                  200,
		"/api/nodes/hk-1/disks":                200,
		"/api/ping/matrix?window=10h":          400,
		"/api/ping/hk-1/jp-1":                  200,
		"/api/ping/nope/jp-1":                  404,
		"/api/traffic":                         200,
		"/api/traffic/hk-1/daily?period=bad":   400,
		"/api/stats":                           200,
		"/api/alerts":                          200,
		"/api/alerts?from=x":                   400,
		"/api/other":                           404,
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
	st.Write("hk-1", &pb.Report{Ts: now.Unix(), Cpu: &pb.CPU{Usage: 50},
		Traffic: []*pb.IfaceTraffic{{Iface: "eth0", Cur: &pb.Period{Start: "2026-09-01", Rx: 10, Tx: 20}}}}, now)

	var nodes []struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		Online  bool    `json:"online"`
		QuotaGB float64 `json:"traffic_quota_gb"`
		Status  *struct {
			CPU     *float64 `json:"cpu"`
			Traffic *struct{ RX, TX int64 }
		} `json:"status"`
	}
	rec := get(t, h, "GET", "/api/nodes")
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != "hk-1" || nodes[0].Name != "香港" || !nodes[0].Online || nodes[0].QuotaGB != 1000 {
		t.Fatalf("nodes: %s", rec.Body)
	}
	if *nodes[0].Status.CPU != 50 || nodes[0].Status.Traffic.TX != 20 {
		t.Fatalf("status: %s", rec.Body)
	}
	if nodes[1].Online || nodes[1].Status != nil || nodes[1].Name != "jp-1" {
		t.Fatalf("silent node: %s", rec.Body)
	}

	rec = get(t, h, "GET", "/api/traffic/hk-1/daily")
	if !strings.Contains(rec.Body.String(), `"period":"2026-09-01"`) {
		t.Fatalf("daily: %s", rec.Body)
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
