// Package api serves the read-only JSON API. It registers GET routes only;
// nothing reachable over HTTP can change data or configuration.
//
// Responses say what the page shows: what counts against a quota, whether a
// node is online and how an alert's value reads are decided here, once, and
// not again in the browser.
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

	"github.com/shakespark/vps-probe/internal/server/alert"
	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
	"github.com/shakespark/vps-probe/internal/wire"
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
	Rules() []alert.RuleView
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
	// Every node at a glance.
	mux.HandleFunc("GET /api/nodes", a.nodes)
	mux.HandleFunc("GET /api/sparks", a.sparks)
	mux.HandleFunc("GET /api/traffic", a.traffic)
	// One node's history; ?from&to in unix seconds, the last hour by default.
	mux.HandleFunc("GET /api/nodes/{id}/metrics", series(a, (*store.Store).Metrics))
	mux.HandleFunc("GET /api/nodes/{id}/net", series(a, (*store.Store).Net))
	mux.HandleFunc("GET /api/nodes/{id}/disks", series(a, (*store.Store).Disks))
	mux.HandleFunc("GET /api/nodes/{id}/ping", series(a, (*store.Store).Pings))
	mux.HandleFunc("GET /api/nodes/{id}/ping/{dst}", a.ping)
	mux.HandleFunc("GET /api/nodes/{id}/traffic/daily", a.daily)
	// Every link.
	mux.HandleFunc("GET /api/ping/matrix", a.matrix)
	mux.HandleFunc("GET /api/ping/availability", a.availability)

	mux.HandleFunc("GET /api/alerts", a.alertsView)
	mux.HandleFunc("GET /api/stats", a.stats)
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
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Region string        `json:"region,omitempty"`
	Group  string        `json:"group,omitempty"`
	Online bool          `json:"online"`
	Plan   *planView     `json:"plan"`   // null without an expiry date
	Quota  *quotaView    `json:"quota"`  // null when unlimited
	Status *store.Status `json:"status"` // null if it never reported
}

// planView is the node's plan as of now: the next expiry date (rolled
// forward for a renewing plan) and the days left in the server's timezone
// (0 = today, negative = past).
type planView struct {
	ExpireAt    string `json:"expire_at"`
	Days        int    `json:"days"`
	RenewMonths int    `json:"renew_months,omitempty"`
	Price       string `json:"price,omitempty"`
}

// quotaView is a node's allowance and what the current period has used of
// it, counted the way the quota's mode says.
type quotaView struct {
	Bytes int64  `json:"bytes"`
	Mode  string `json:"mode"` // in words, e.g. "收+发"
	Used  int64  `json:"used"`
}

func quota(n *config.Node, cur *store.Period) *quotaView {
	if n.Traffic.Quota() == 0 {
		return nil
	}
	q := &quotaView{Bytes: n.Traffic.Quota(), Mode: n.Traffic.QuotaModeText()}
	if cur != nil {
		q.Used = n.Traffic.Billable(cur.RX, cur.TX)
	}
	return q
}

func (a *API) nodes(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	out := make([]nodeView, 0, len(a.cfg.Nodes))
	for i := range a.cfg.Nodes {
		n := &a.cfg.Nodes[i]
		st, err := a.store.Status(r.Context(), n.ID)
		if err != nil {
			a.fail(w, err)
			return
		}
		v := nodeView{ID: n.ID, Name: n.Name, Region: n.Region, Group: n.Group, Online: st.Online(now), Status: st}
		if date, days, ok := n.Plan.Expiry(now, a.cfg.Location); ok {
			v.Plan = &planView{ExpireAt: date, Days: days, RenewMonths: n.Plan.RenewMonths, Price: n.Plan.Price}
		}
		var cur *store.Period
		if st != nil {
			cur = st.Traffic
		}
		v.Quota = quota(n, cur)
		out = append(out, v)
	}
	writeJSON(w, out)
}

// Sparklines on the overview: the last hour, a point a minute. The last
// bucket contains now.
const (
	sparkStep = 60
	sparkN    = 60
)

func (a *API) sparks(w http.ResponseWriter, r *http.Request) {
	from := (a.now().Unix()/sparkStep+1)*sparkStep - sparkN*sparkStep
	all, err := a.store.Sparks(r.Context(), from, sparkStep, sparkN)
	if err != nil {
		a.fail(w, err)
		return
	}
	// Only nodes still in the config, like every other endpoint.
	nodes := map[string]*store.Spark{}
	for _, n := range a.cfg.Nodes {
		if sp, ok := all[n.ID]; ok {
			nodes[n.ID] = sp
		}
	}
	writeJSON(w, map[string]any{"from": from, "step": sparkStep, "n": sparkN, "nodes": nodes})
}

// series serves one kind of a node's history over ?from&to.
func series[T any](a *API, query func(*store.Store, context.Context, string, int64, int64) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.node(w, r)
		if !ok {
			return
		}
		from, to, ok := a.timeRange(w, r, defaultRange)
		if !ok {
			return
		}
		v, err := query(a.store, r.Context(), id, from, to)
		a.reply(w, v, err)
	}
}

func (a *API) ping(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	from, to, ok := a.timeRange(w, r, defaultRange)
	if !ok {
		return
	}
	s, err := a.store.Ping(r.Context(), id, r.PathValue("dst"), from, to)
	a.reply(w, s, err)
}

// Availability ranges: cells of whole 5m periods, about 50-60 per strip.
var availRanges = map[string]struct {
	cell int64
	n    int
}{
	"24h": {1800, 48},
	"7d":  {3 * 3600, 56},
	"30d": {12 * 3600, 60},
}

// availability returns every link's history as a strip of equal cells,
// the last of which contains now.
func (a *API) availability(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	spec, ok := availRanges[rng]
	if !ok {
		http.Error(w, "range: want 24h, 7d or 30d", http.StatusBadRequest)
		return
	}
	from := (a.now().Unix()/spec.cell+1)*spec.cell - int64(spec.n)*spec.cell
	links, err := a.store.Availability(r.Context(), from, spec.cell, spec.n)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"range": rng, "from": from, "cell": spec.cell, "n": spec.n,
		"down_loss_pct": store.DownLossPct, "links": links})
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
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Quota   *quotaView   `json:"quota"`   // null when unlimited
	Periods []periodView `json:"periods"` // newest first
}

type periodView struct {
	store.Period
	Billable int64 `json:"billable"` // what counts against the quota
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
	for i := range a.cfg.Nodes {
		n := &a.cfg.Nodes[i]
		ps, err := a.store.Periods(r.Context(), n.ID, limit)
		if err != nil {
			a.fail(w, err)
			return
		}
		v := trafficView{ID: n.ID, Name: n.Name, Quota: quota(n, nil), Periods: make([]periodView, len(ps))}
		for i, p := range ps {
			v.Periods[i] = periodView{p, n.Traffic.Billable(p.RX, p.TX)}
		}
		if v.Quota != nil && len(ps) > 0 {
			v.Quota.Used = v.Periods[0].Billable
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

// daily: ?period=<start, unix seconds>; the current period by default.
func (a *API) daily(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	var period int64
	if v := r.URL.Query().Get("period"); v != "" {
		var err error
		if period, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "period: want its start in unix seconds", http.StatusBadRequest)
			return
		}
	} else {
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
	}
	days, err := a.store.Daily(r.Context(), id, period)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"period": period, "days": days})
}

const (
	maxAlertHistory   = 500
	alertHistoryRange = 7 * 24 * time.Hour
)

// alertsView: current alerts, history in ?from&to (default: the last 7 days;
// newest first, at most 500) narrowed by ?node, ?rule and ?event (comma-
// separated), the nodes and rules with history in the range (for filter
// menus), and the rules in effect.
func (a *API) alertsView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, ok := a.timeRange(w, r, alertHistoryRange)
	if !ok {
		return
	}
	f := store.AlertFilter{Node: q.Get("node"), Rule: q.Get("rule")}
	if v := q.Get("event"); v != "" {
		f.Events = strings.Split(v, ",")
		for _, e := range f.Events {
			if !slices.Contains(alert.Events, e) {
				http.Error(w, "event: want a comma-separated list of "+strings.Join(alert.Events, ", "), http.StatusBadRequest)
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
		"rules":     a.alerts.Rules(), "channels": a.cfg.ChannelNames()})
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	size, err := a.store.Size(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ingest": a.ingest.Stats(), "db_bytes": size, "server_time": a.now().Unix(),
		"timezone": a.cfg.Timezone, "version": a.version, "interval": int(wire.Interval / time.Second)})
}

// node returns the {id} of the path, or answers 404 if no such node is
// configured.
func (a *API) node(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, known := a.cfg.Node(id); !known {
		http.NotFound(w, r)
		return "", false
	}
	return id, true
}

// timeRange parses ?from&to (unix seconds). to defaults to now, from to
// span before to.
func (a *API) timeRange(w http.ResponseWriter, r *http.Request, span time.Duration) (from, to int64, ok bool) {
	q := r.URL.Query()
	to = a.now().Unix()
	var err error
	if v := q.Get("to"); v != "" {
		if to, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "to: want unix seconds", http.StatusBadRequest)
			return 0, 0, false
		}
	}
	from = to - int64(span/time.Second)
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
