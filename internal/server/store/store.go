// Package store keeps everything the server knows in one SQLite file:
// metrics, rollups, traffic totals and node status. Backing up or migrating
// the server means copying that file (plus server.yml for the tokens).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, keeps CGO_ENABLED=0 builds
)

// schemaVersion 10 is the 0.2 schema. Versions below it are 0.1.x files,
// which are not carried over.
const schemaVersion = 10

// Rollup buckets, in seconds.
const (
	step5m = 300
	step1h = 3600
)

type Store struct {
	w   *sql.DB // single connection: ingest, rollups and cleanup
	r   *sql.DB // read-only pool for the web API
	loc *time.Location
	ret Retention
	log *slog.Logger
	now func() time.Time

	path string

	mu    sync.RWMutex
	nodes map[string]int64 // config id -> row id
	names map[int64]string
}

type Options struct {
	Location  *time.Location // day boundaries for daily traffic
	Retention Retention
	Log       *slog.Logger
}

// Retention is how long each resolution is kept.
type Retention struct {
	Raw, M5, H1 time.Duration
}

// Open creates or opens the database at path.
func Open(path string, opt Options) (*Store, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)

	s := &Store{w: w, loc: opt.Location, ret: opt.Retention, log: opt.Log, now: time.Now, path: path}
	if err := s.migrate(); err != nil {
		w.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	// Opened after migrate so the file and schema exist.
	if s.r, err = sql.Open("sqlite", dsn(path, true)); err != nil {
		w.Close()
		return nil, err
	}
	s.r.SetMaxOpenConns(4)
	return s, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Close folds the WAL back into the main file, so a stopped server leaves
// exactly one file behind.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if _, err := s.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		errs = append(errs, fmt.Errorf("checkpoint: %w", err))
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}

// migrate creates the schema in a new file and checks the version of an
// existing one. Later schema changes become further steps here.
func (s *Store) migrate() error {
	var v int
	err := s.w.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'").Scan(&v)
	if err != nil {
		v = 0 // a new file
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("schema version %d is newer than this build (%d); use a newer vps-probe-server", v, schemaVersion)
	case v != 0:
		return errors.New("this database was written by vps-probe 0.1.x, which 0.2 cannot read; move the file away and start a new one")
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range schema {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%w in %.60q", err, stmt)
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('created_at', ?), ('schema_version', ?)`,
		time.Now().UTC().Format(time.RFC3339), strconv.Itoa(schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Metrics come in three resolutions: raw (one row per report), 5m and 1h
// rollups. A column is NULL when the agent could not read that source.
var schema = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,

	// max_ts: newest report ts seen. fresh_at: server time when a report
	// raised max_ts; replays of old packets never do, so they can't make a
	// dead node look alive. ip: the source of that newest report, and since
	// when; a replay from elsewhere can't change it either. The sys_ columns
	// are the newest system description (sent on start and hourly).
	`CREATE TABLE node_status (
		node     INTEGER PRIMARY KEY,
		max_ts   INTEGER NOT NULL,
		fresh_at INTEGER NOT NULL,
		skew     INTEGER NOT NULL,
		ip       TEXT,
		ip_since INTEGER,
		sys_ts   INTEGER,
		hostname TEXT, os TEXT, kernel TEXT, arch TEXT, cores INTEGER, boot_time INTEGER, agent_version TEXT
	)`,

	`CREATE TABLE metrics_raw (
		node INTEGER NOT NULL, ts INTEGER NOT NULL,
		cpu REAL, steal REAL, softirq REAL, load1 REAL, load5 REAL, load15 REAL,
		mem_total INTEGER, mem_used INTEGER, swap_total INTEGER, swap_used INTEGER,
		tcp INTEGER, udp INTEGER, tcp_tw INTEGER, threads INTEGER,
		PRIMARY KEY (node, ts)
	) WITHOUT ROWID`,
	rollupMetrics("metrics_5m"),
	rollupMetrics("metrics_1h"),

	`CREATE TABLE net_raw (
		node INTEGER NOT NULL, iface TEXT NOT NULL, ts INTEGER NOT NULL,
		rx REAL NOT NULL, tx REAL NOT NULL, rx_pps REAL NOT NULL, tx_pps REAL NOT NULL,
		PRIMARY KEY (node, iface, ts)
	) WITHOUT ROWID`,
	rollupNet("net_5m"),
	rollupNet("net_1h"),

	`CREATE TABLE disk_raw (
		node INTEGER NOT NULL, mount TEXT NOT NULL, ts INTEGER NOT NULL,
		total INTEGER NOT NULL, used INTEGER NOT NULL, avail INTEGER NOT NULL, inode_pct REAL NOT NULL,
		PRIMARY KEY (node, mount, ts)
	) WITHOUT ROWID`,
	`CREATE TABLE disk_1h (
		node INTEGER NOT NULL, mount TEXT NOT NULL, ts INTEGER NOT NULL,
		total INTEGER NOT NULL, used REAL NOT NULL, used_max INTEGER NOT NULL, avail REAL NOT NULL, inode_pct REAL NOT NULL,
		PRIMARY KEY (node, mount, ts)
	) WITHOUT ROWID`,

	// min/avg/max/jitter are NULL when no reply came back.
	pingTable("ping_raw"),
	pingTable("ping_5m"),
	pingTable("ping_1h"),

	// Totals per billing period [start, end). ts is the report that wrote
	// them: only a newer report may overwrite.
	`CREATE TABLE traffic_period (
		node INTEGER NOT NULL, iface TEXT NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL,
		rx INTEGER NOT NULL, tx INTEGER NOT NULL, ts INTEGER NOT NULL,
		PRIMARY KEY (node, iface, start)
	) WITHOUT ROWID`,
	// The current period's total as last seen on each day.
	`CREATE TABLE traffic_daily (
		node INTEGER NOT NULL, iface TEXT NOT NULL, day TEXT NOT NULL, start INTEGER NOT NULL,
		rx INTEGER NOT NULL, tx INTEGER NOT NULL, ts INTEGER NOT NULL,
		PRIMARY KEY (node, iface, day)
	) WITHOUT ROWID`,

	// Alerts that are pending or firing, so a restart neither repeats nor
	// forgets them. target: a mount point or peer name, or "".
	`CREATE TABLE alert_state (
		rule TEXT NOT NULL, node TEXT NOT NULL, target TEXT NOT NULL,
		firing INTEGER NOT NULL, since INTEGER NOT NULL, notified INTEGER NOT NULL, value REAL NOT NULL,
		PRIMARY KEY (rule, node, target)
	) WITHOUT ROWID`,
	// What each report last sent for a node ("" = all nodes), so that it is
	// sent once: see ReportMark.
	`CREATE TABLE report_state (
		report TEXT NOT NULL, node TEXT NOT NULL,
		mark TEXT NOT NULL, level REAL NOT NULL, at INTEGER NOT NULL,
		PRIMARY KEY (report, node)
	) WITHOUT ROWID`,
	// Everything that was sent or would have been. rule is an alert rule's
	// name or a report's type.
	`CREATE TABLE alert_history (
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
		rule TEXT NOT NULL, node TEXT NOT NULL, target TEXT NOT NULL,
		event TEXT NOT NULL, value TEXT NOT NULL, message TEXT NOT NULL
	)`,
	`CREATE INDEX alert_history_ts ON alert_history (ts)`,
}

func rollupMetrics(name string) string {
	return `CREATE TABLE ` + name + ` (
		node INTEGER NOT NULL, ts INTEGER NOT NULL,
		cpu REAL, cpu_max REAL, steal REAL, steal_max REAL, softirq REAL, softirq_max REAL,
		load1 REAL, load1_max REAL, load5 REAL, load15 REAL,
		mem_total INTEGER, mem_used REAL, mem_used_max INTEGER,
		swap_total INTEGER, swap_used REAL, swap_used_max INTEGER,
		tcp REAL, tcp_max INTEGER, udp REAL, udp_max INTEGER, tcp_tw REAL,
		threads REAL, threads_max INTEGER,
		PRIMARY KEY (node, ts)
	) WITHOUT ROWID`
}

func rollupNet(name string) string {
	return `CREATE TABLE ` + name + ` (
		node INTEGER NOT NULL, iface TEXT NOT NULL, ts INTEGER NOT NULL,
		rx REAL NOT NULL, rx_max REAL NOT NULL, tx REAL NOT NULL, tx_max REAL NOT NULL,
		rx_pps REAL NOT NULL, rx_pps_max REAL NOT NULL, tx_pps REAL NOT NULL, tx_pps_max REAL NOT NULL,
		PRIMARY KEY (node, iface, ts)
	) WITHOUT ROWID`
}

func pingTable(name string) string {
	return `CREATE TABLE ` + name + ` (
		src INTEGER NOT NULL, dst TEXT NOT NULL, ts INTEGER NOT NULL,
		sent INTEGER NOT NULL, lost INTEGER NOT NULL,
		min REAL, avg REAL, max REAL, jitter REAL,
		PRIMARY KEY (src, dst, ts)
	) WITHOUT ROWID`
}

// pingAvg averages the avg column over rows, weighted by replies received.
// It works on raw rows and on rollups alike, since rollups keep the counts.
const pingAvg = `sum(avg * (sent - lost)) / NULLIF(sum(CASE WHEN avg IS NULL THEN 0 ELSE sent - lost END), 0)`

// SyncNodes makes sure every configured node has a row id. Data of nodes
// removed from the config stays in the file but is no longer shown.
func (s *Store) SyncNodes(ids []string) error {
	nodes := make(map[string]int64, len(ids))
	names := make(map[int64]string, len(ids))
	for _, id := range ids {
		if _, err := s.w.Exec("INSERT OR IGNORE INTO nodes(name) VALUES (?)", id); err != nil {
			return err
		}
		var rid int64
		if err := s.w.QueryRow("SELECT id FROM nodes WHERE name = ?", id).Scan(&rid); err != nil {
			return err
		}
		nodes[id], names[rid] = rid, id
	}
	s.mu.Lock()
	s.nodes, s.names = nodes, names
	s.mu.Unlock()
	return nil
}

func (s *Store) nodeID(id string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rid, ok := s.nodes[id]
	return rid, ok
}

func (s *Store) nodeName(rid int64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.names[rid]
	return n, ok
}

// Size returns the logical database size in bytes.
func (s *Store) Size(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.r.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.r.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}
