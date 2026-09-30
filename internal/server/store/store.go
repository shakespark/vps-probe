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

const schemaVersion = 2

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

// migrate brings the file to schemaVersion, one step at a time, each step
// in its own transaction. Steps only add tables, so older data is kept.
func (s *Store) migrate() error {
	var v int
	err := s.w.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'").Scan(&v)
	if err != nil {
		v = 0 // fresh database (or no meta table yet)
	}
	if v > schemaVersion {
		return fmt.Errorf("schema version %d is newer than this build (%d); use a newer vps-probe-server", v, schemaVersion)
	}
	for ; v < schemaVersion; v++ {
		if err := s.step(v+1, migrations[v]); err != nil {
			return fmt.Errorf("migrating to schema %d: %w", v+1, err)
		}
		if v > 0 && s.log != nil {
			s.log.Info("database schema upgraded", "version", v+1)
		}
	}
	return nil
}

func (s *Store) step(version int, stmts []string) error {
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%w in %.60q", err, stmt)
		}
	}
	if version == 1 {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES ('created_at', ?)`,
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', ?)`,
		strconv.Itoa(version)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrations[i] takes the schema from version i to i+1.
var migrations = [][]string{schemaV1, schemaV2}

var schemaV2 = []string{
	// Firing alerts and notified traffic levels, so a restart neither
	// repeats nor forgets them. target: mount, peer name or period start.
	`CREATE TABLE IF NOT EXISTS alert_state (
		rule TEXT NOT NULL, node TEXT NOT NULL, target TEXT NOT NULL,
		state TEXT NOT NULL, since INTEGER NOT NULL, notified INTEGER NOT NULL, value REAL,
		PRIMARY KEY (rule, node, target)
	) WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS alert_history (
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
		rule TEXT NOT NULL, node TEXT NOT NULL, target TEXT NOT NULL,
		event TEXT NOT NULL, value REAL, message TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS alert_history_ts ON alert_history (ts)`,
}

var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,

	// max_ts: newest report ts seen. fresh_at: server time when a report
	// raised max_ts; replays of old packets never do, so they can't make a
	// dead node look alive.
	`CREATE TABLE IF NOT EXISTS node_status (
		node     INTEGER PRIMARY KEY,
		max_ts   INTEGER NOT NULL,
		fresh_at INTEGER NOT NULL,
		skew     INTEGER NOT NULL,
		sys      TEXT,
		sys_ts   INTEGER
	)`,

	`CREATE TABLE IF NOT EXISTS metrics_raw (
		node INTEGER NOT NULL, ts INTEGER NOT NULL,
		cpu REAL, steal REAL, load1 REAL, load5 REAL, load15 REAL,
		mem_total INTEGER, mem_used INTEGER, swap_total INTEGER, swap_used INTEGER,
		PRIMARY KEY (node, ts)
	) WITHOUT ROWID`,
	rollupMetrics("metrics_5m"),
	rollupMetrics("metrics_1h"),

	`CREATE TABLE IF NOT EXISTS net_raw (
		node INTEGER NOT NULL, iface TEXT NOT NULL, ts INTEGER NOT NULL,
		rx REAL NOT NULL, tx REAL NOT NULL,
		PRIMARY KEY (node, iface, ts)
	) WITHOUT ROWID`,
	rollupNet("net_5m"),
	rollupNet("net_1h"),

	`CREATE TABLE IF NOT EXISTS disk_raw (
		node INTEGER NOT NULL, mount TEXT NOT NULL, ts INTEGER NOT NULL,
		total INTEGER NOT NULL, used INTEGER NOT NULL, avail INTEGER NOT NULL, inode_pct REAL NOT NULL,
		PRIMARY KEY (node, mount, ts)
	) WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS disk_1h (
		node INTEGER NOT NULL, mount TEXT NOT NULL, ts INTEGER NOT NULL,
		total INTEGER NOT NULL, used REAL NOT NULL, used_max INTEGER NOT NULL, avail REAL NOT NULL, inode_pct REAL NOT NULL,
		PRIMARY KEY (node, mount, ts)
	) WITHOUT ROWID`,

	// min/avg/max/jitter are NULL when no reply came back.
	`CREATE TABLE IF NOT EXISTS ping_raw (
		src INTEGER NOT NULL, dst TEXT NOT NULL, ts INTEGER NOT NULL,
		sent INTEGER NOT NULL, lost INTEGER NOT NULL,
		min REAL, avg REAL, max REAL, jitter REAL,
		PRIMARY KEY (src, dst, ts)
	) WITHOUT ROWID`,
	rollupPing("ping_5m"),
	rollupPing("ping_1h"),

	// Totals per billing period. ts is the report that wrote them: only a
	// newer report may overwrite (see DESIGN.md §4.4).
	`CREATE TABLE IF NOT EXISTS traffic_period (
		node INTEGER NOT NULL, iface TEXT NOT NULL, start TEXT NOT NULL,
		rx INTEGER NOT NULL, tx INTEGER NOT NULL, ts INTEGER NOT NULL,
		PRIMARY KEY (node, iface, start)
	) WITHOUT ROWID`,
	// The current period's total as last seen on each day.
	`CREATE TABLE IF NOT EXISTS traffic_daily (
		node INTEGER NOT NULL, iface TEXT NOT NULL, day TEXT NOT NULL, start TEXT NOT NULL,
		rx INTEGER NOT NULL, tx INTEGER NOT NULL, ts INTEGER NOT NULL,
		PRIMARY KEY (node, iface, day)
	) WITHOUT ROWID`,
}

func rollupMetrics(name string) string {
	return `CREATE TABLE IF NOT EXISTS ` + name + ` (
		node INTEGER NOT NULL, ts INTEGER NOT NULL,
		cpu REAL, cpu_max REAL, steal REAL, steal_max REAL,
		load1 REAL, load1_max REAL, load5 REAL, load15 REAL,
		mem_total INTEGER, mem_used REAL, mem_used_max INTEGER,
		swap_total INTEGER, swap_used REAL, swap_used_max INTEGER,
		PRIMARY KEY (node, ts)
	) WITHOUT ROWID`
}

func rollupNet(name string) string {
	return `CREATE TABLE IF NOT EXISTS ` + name + ` (
		node INTEGER NOT NULL, iface TEXT NOT NULL, ts INTEGER NOT NULL,
		rx REAL NOT NULL, rx_max REAL NOT NULL, tx REAL NOT NULL, tx_max REAL NOT NULL,
		PRIMARY KEY (node, iface, ts)
	) WITHOUT ROWID`
}

func rollupPing(name string) string {
	return `CREATE TABLE IF NOT EXISTS ` + name + ` (
		src INTEGER NOT NULL, dst TEXT NOT NULL, ts INTEGER NOT NULL,
		sent INTEGER NOT NULL, lost INTEGER NOT NULL,
		min REAL, avg REAL, max REAL, jitter REAL,
		PRIMARY KEY (src, dst, ts)
	) WITHOUT ROWID`
}

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
