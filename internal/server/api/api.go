// Package api serves the read-only JSON API. It registers GET routes only;
// nothing reachable over HTTP can change data or configuration.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"vpsprobe/internal/server/alert"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/server/store"
)

const (
	defaultRange = time.Hour
	maxRange     = 400 * 24 * time.Hour
	maxWindow    = time.Hour
	maxPeriods   = 24
)

type Stats interface {
	Stats() map[string]uint64
}

// Alerts exposes the evaluator's current state.
type Alerts interface {
	Active() []alert.Active
}

type API struct {
	cfg     *config.Config
	store   *store.Store
	ingest  Stats
	alerts  Alerts
	version string
	log     *slog.Logger
	now     func() time.Time
}

func New(cfg *config.Config, st *store.Store, ingest Stats, alerts Alerts, version string, log *slog.Logger) *API {
	return &API{cfg: cfg, store: st, ingest: ingest, alerts: alerts, version: version, log: log, now: time.Now}
}

// CSP for the UI: no inline script or style, no third-party origins, no
// framing. Charts use ECharts' canvas (richText) tooltips so no inline
// styles are needed.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Handler returns the routes. GET patterns also answer HEAD; any other
// method gets 405 from the mux. ui, if set, serves "/" and "/static/";
// every other path is 404 (hash routing needs no fallback).
func (a *API) Handler(ui http.Handler) http.Handler {
	mux := http.NewServeMux()
	if ui != nil {
		mux.Handle("GET /{$}", ui)
		mux.Handle("GET /static/", ui)
	}
	mux.HandleFunc("GET /api/nodes", a.nodes)
	mux.HandleFunc("GET /api/nodes/{id}/metrics", a.metrics)
	mux.HandleFunc("GET /api/nodes/{id}/net", a.net)
	mux.HandleFunc("GET /api/nodes/{id}/disks", a.disks)
	mux.HandleFunc("GET /api/ping/matrix", a.matrix)
	mux.HandleFunc("GET /api/ping/{src}/{dst}", a.ping)
	mux.HandleFunc("GET /api/traffic", a.traffic)
	mux.HandleFunc("GET /api/traffic/{id}/daily", a.daily)
	mux.HandleFunc("GET /api/stats", a.stats)
	mux.HandleFunc("GET /api/alerts", a.alertsView)
	return secure(mux)
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", csp)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hd.Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

type nodeView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Online    bool    `json:"online"`
	QuotaGB   float64 `json:"traffic_quota_gb,omitempty"`
	QuotaMode string  `json:"traffic_quota_mode"`
	// Next expiry date (rolled forward for renew_months) and days left
	// in the server timezone; absent without expire_at.
	ExpireAt    string        `json:"expire_at,omitempty"`
	ExpireDays  *int          `json:"expire_days,omitempty"`
	RenewMonths int           `json:"renew_months,omitempty"`
	Price       string        `json:"price,omitempty"`
	Status      *store.Status `json:"status"` // null if it never reported
}

func (a *API) nodes(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	out := make([]nodeView, 0, len(a.cfg.Nodes))
	for _, n := range a.cfg.Nodes {
		st, err := a.store.Status(r.Context(), n.ID)
		if err != nil {
			a.fail(w, err)
			return
		}
		v := nodeView{ID: n.ID, Name: n.Name, QuotaGB: n.QuotaGB, QuotaMode: n.QuotaMode,
			RenewMonths: n.RenewMonths, Price: n.Price, Status: st}
		if date, days, ok := n.Expiry(now, a.cfg.Location); ok {
			v.ExpireAt, v.ExpireDays = date, &days
		}
		if st != nil {
			v.Online = now.Sub(time.Unix(st.FreshAt, 0)) < time.Duration(a.cfg.OfflineAfter)
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	id, from, to, ok := a.nodeRange(w, r)
	if !ok {
		return
	}
	s, err := a.store.Metrics(r.Context(), id, from, to)
	a.reply(w, s, err)
}

func (a *API) net(w http.ResponseWriter, r *http.Request) {
	id, from, to, ok := a.nodeRange(w, r)
	if !ok {
		return
	}
	s, err := a.store.Net(r.Context(), id, from, to)
	a.reply(w, s, err)
}

func (a *API) disks(w http.ResponseWriter, r *http.Request) {
	id, from, to, ok := a.nodeRange(w, r)
	if !ok {
		return
	}
	s, err := a.store.Disks(r.Context(), id, from, to)
	a.reply(w, s, err)
}

func (a *API) ping(w http.ResponseWriter, r *http.Request) {
	src, dst := r.PathValue("src"), r.PathValue("dst")
	if _, ok := a.cfg.Node(src); !ok {
		http.NotFound(w, r)
		return
	}
	from, to, ok := a.timeRange(w, r)
	if !ok {
		return
	}
	s, err := a.store.Ping(r.Context(), src, dst, from, to)
	a.reply(w, s, err)
}

func (a *API) matrix(w http.ResponseWriter, r *http.Request) {
	window := 5 * time.Minute
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > maxWindow {
			http.Error(w, "window: want a duration up to 1h", http.StatusBadRequest)
			return
		}
		window = d
	}
	links, err := a.store.Matrix(r.Context(), window)
	if err != nil {
		a.fail(w, err)
		return
	}
	nodes := make([]string, len(a.cfg.Nodes))
	for i, n := range a.cfg.Nodes {
		nodes[i] = n.ID
	}
	writeJSON(w, map[string]any{"window": window.String(), "nodes": nodes, "links": links})
}

type trafficView struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	QuotaGB   float64        `json:"traffic_quota_gb,omitempty"`
	QuotaMode string         `json:"traffic_quota_mode"`
	Periods   []store.Period `json:"periods"` // newest first
}

func (a *API) traffic(w http.ResponseWriter, r *http.Request) {
	limit := maxPeriods
	if v := r.URL.Query().Get("periods"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPeriods {
			http.Error(w, "periods: want 1-24", http.StatusBadRequest)
			return
		}
		limit = n
	}
	out := make([]trafficView, 0, len(a.cfg.Nodes))
	for _, n := range a.cfg.Nodes {
		p, err := a.store.Periods(r.Context(), n.ID, limit)
		if err != nil {
			a.fail(w, err)
			return
		}
		out = append(out, trafficView{ID: n.ID, Name: n.Name, QuotaGB: n.QuotaGB, QuotaMode: n.QuotaMode, Periods: p})
	}
	writeJSON(w, out)
}

// daily: ?period=YYYY-MM-DD (period start); defaults to the current period.
func (a *API) daily(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.cfg.Node(id); !ok {
		http.NotFound(w, r)
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		p, err := a.store.Periods(r.Context(), id, 1)
		if err != nil {
			a.fail(w, err)
			return
		}
		if len(p) == 0 {
			writeJSON(w, map[string]any{"period": nil, "days": []store.DayTraffic{}})
			return
		}
		period = p[0].Start
	} else if _, err := time.Parse(time.DateOnly, period); err != nil {
		http.Error(w, "period: want YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	days, err := a.store.Daily(r.Context(), id, period)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"period": period, "days": days})
}

const maxAlertHistory = 500

var alertEvents = []string{"firing", "repeat", "recovered", "level", "changed"}

// alertsView: current alerts, history in ?from&to (default: last 7 days,
// newest first, at most 500) narrowed by ?node, ?rule and ?event (comma-
// separated), the nodes and rules with history in the range (for filter
// menus), and the rules in effect.
func (a *API) alertsView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("from") == "" {
		q.Set("from", strconv.FormatInt(a.now().Add(-7*24*time.Hour).Unix(), 10))
		r.URL.RawQuery = q.Encode()
	}
	from, to, ok := a.timeRange(w, r)
	if !ok {
		return
	}
	f := store.AlertFilter{Node: q.Get("node"), Rule: q.Get("rule")}
	if v := q.Get("event"); v != "" {
		f.Events = strings.Split(v, ",")
		for _, e := range f.Events {
			if !slices.Contains(alertEvents, e) {
				http.Error(w, "event: want a comma-separated list of "+strings.Join(alertEvents, ", "), http.StatusBadRequest)
				return
			}
		}
	}
	hist, err := a.store.AlertHistory(r.Context(), from, to+1, f, maxAlertHistory)
	if err != nil {
		a.fail(w, err)
		return
	}
	nodes, rules, err := a.store.AlertFacets(r.Context(), from, to+1)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"active": a.alerts.Active(), "history": hist,
		"truncated": len(hist) == maxAlertHistory,
		"facets":    map[string]any{"nodes": nodes, "rules": rules},
		"rules":     a.cfg.Alerts, "telegram": a.cfg.Telegram.Enabled()})
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	size, err := a.store.Size(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ingest": a.ingest.Stats(), "db_bytes": size, "server_time": a.now().Unix(),
		"timezone": a.cfg.Timezone, "version": a.version})
}

// nodeRange parses {id} plus ?from&to (unix seconds). Default: the last hour.
func (a *API) nodeRange(w http.ResponseWriter, r *http.Request) (id string, from, to int64, ok bool) {
	id = r.PathValue("id")
	if _, known := a.cfg.Node(id); !known {
		http.NotFound(w, r)
		return "", 0, 0, false
	}
	from, to, ok = a.timeRange(w, r)
	return id, from, to, ok
}

func (a *API) timeRange(w http.ResponseWriter, r *http.Request) (from, to int64, ok bool) {
	q := r.URL.Query()
	to = a.now().Unix()
	var err error
	if v := q.Get("to"); v != "" {
		if to, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "to: want unix seconds", http.StatusBadRequest)
			return 0, 0, false
		}
	}
	from = to - int64(defaultRange/time.Second)
	if v := q.Get("from"); v != "" {
		if from, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "from: want unix seconds", http.StatusBadRequest)
			return 0, 0, false
		}
	}
	if from >= to || to-from > int64(maxRange/time.Second) {
		http.Error(w, "range: want from < to, at most 400 days apart", http.StatusBadRequest)
		return 0, 0, false
	}
	return from, to, true
}

func (a *API) reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, v)
}

// fail logs the detail and returns a generic error; SQL errors stay out of
// responses.
func (a *API) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	a.log.Error("api", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
