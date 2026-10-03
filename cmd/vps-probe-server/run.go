package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shakespark/vps-probe/internal/cli"
	"github.com/shakespark/vps-probe/internal/server/alert"
	"github.com/shakespark/vps-probe/internal/server/api"
	"github.com/shakespark/vps-probe/internal/server/basicauth"
	"github.com/shakespark/vps-probe/internal/server/cfaccess"
	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/ingest"
	"github.com/shakespark/vps-probe/internal/server/notify"
	"github.com/shakespark/vps-probe/internal/server/store"
	"github.com/shakespark/vps-probe/web"
)

const (
	rollupEvery   = time.Minute
	cleanupEvery  = time.Hour
	backupHour    = 4 // local time; the daily backup runs after this hour
	shutdownGrace = 10 * time.Second
)

func openStore(cfg *config.Config, log *slog.Logger) (*store.Store, error) {
	return store.Open(cfg.DB, store.Options{Location: cfg.Location, Log: log, Retention: store.Retention{
		Raw: time.Duration(cfg.Retention.Raw),
		M5:  time.Duration(cfg.Retention.M5),
		H1:  time.Duration(cfg.Retention.H1),
	}})
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := configFlag(fs)
	debug := fs.Bool("debug", false, "debug logging")
	fs.Parse(args)
	log := cli.Logger(*debug)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	st, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing database", "err", err)
		} else {
			log.Info("database closed")
		}
	}()
	if len(cfg.Nodes) == 0 {
		log.Warn("no nodes in the config: add the first with `vps-probe-server add-node -id ID`, then restart")
	}
	ids := make([]string, len(cfg.Nodes))
	for i, n := range cfg.Nodes {
		ids[i] = n.ID
	}
	if err := st.SyncNodes(ids); err != nil {
		return err
	}
	// Catch up on buckets left open by a previous shutdown or crash.
	if err := st.Rollup(time.Now().Add(-time.Duration(cfg.Retention.Raw))); err != nil {
		return err
	}
	if err := st.Cleanup(); err != nil {
		return err
	}

	in, err := ingest.Listen(cfg.Listen.Ingest, cfg.Nodes, st, log)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	channels := make([]*notify.Channel, len(cfg.Notify))
	var notifier notify.Notifier = notify.Log{Log: log}
	if len(channels) > 0 {
		all := make(notify.Multi, len(channels))
		for i, c := range cfg.Notify {
			channels[i] = notify.New(c, log)
			all[i] = channels[i]
		}
		notifier = all
	} else {
		log.Warn("no notify channel configured: alerts are recorded and logged but not sent")
	}
	ev := alert.New(cfg, st, notifier, log)
	if err := ev.Load(); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen.Web)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	warnIfExposed(cfg, log)
	ui, err := web.New()
	if err != nil {
		return fmt.Errorf("web ui: %w", err)
	}
	handler := api.New(cfg, st, in, ev, version, log).Handler(ui)
	var access *cfaccess.Verifier
	if cfg.CFAccess.Enabled() {
		access = cfaccess.New(cfg.CFAccess.TeamDomain, cfg.CFAccess.AUD, log)
		handler = access.Middleware(handler)
	}
	if cfg.BasicAuth.Enabled() {
		handler = basicauth.New(cfg.BasicAuth.User, cfg.BasicAuth.PasswordHash, log).Middleware(handler)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := cli.Context()
	defer stop()

	// Everything that writes to the database is waited for before it closes.
	var writers sync.WaitGroup
	writers.Add(4)
	go func() { defer writers.Done(); ev.Run(ctx) }()
	go func() { defer writers.Done(); in.Run(ctx) }()
	go func() { defer writers.Done(); maintain(ctx, cfg, st, log) }()
	go func() {
		defer writers.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web", "err", err)
			stop()
		}
	}()
	// Not waited for: an HTTP request in flight must not delay shutdown.
	for _, c := range channels {
		go c.Run(ctx)
	}
	if access != nil {
		go access.Run(ctx)
	}
	log.Info("server started", "version", version, "ingest", in.Addr(), "web", ln.Addr(),
		"nodes", len(cfg.Nodes), "db", cfg.DB, "alerts", len(cfg.Alerts), "reports", len(cfg.Reports),
		"notify", strings.Join(cfg.ChannelNames(), ","), "cf_access", cfg.CFAccess.Enabled(), "basic_auth", cfg.BasicAuth.Enabled())

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	srv.Shutdown(sctx)
	writers.Wait()
	return nil
}

// warnIfExposed speaks up when the web UI listens beyond loopback without
// what that needs.
func warnIfExposed(cfg *config.Config, log *slog.Logger) {
	host, _, _ := net.SplitHostPort(cfg.Listen.Web)
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return
	}
	switch {
	case cfg.BasicAuth.Enabled():
		log.Warn("web UI is not bound to loopback: basic_auth sends the password with every request, serve it only through an HTTPS reverse proxy",
			"listen", cfg.Listen.Web)
	case !cfg.CFAccess.Enabled():
		log.Warn("web UI is not bound to loopback and neither cf_access nor basic_auth is set: anyone who can reach it sees everything",
			"listen", cfg.Listen.Web)
	}
}

// maintain runs rollups, cleanup and the daily backup. It shares the single
// writer connection with ingest, so each step briefly delays report writes.
func maintain(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(rollupEvery)
	defer t.Stop()
	// Zero, so the first tick also cleans up and checks the daily backup:
	// a server restarted more often than hourly still gets its backup.
	var lastCleanup time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := st.Rollup(now.Add(-store.RollupWindow)); err != nil {
				log.Error("rollup", "err", err)
			}
			if now.Sub(lastCleanup) < cleanupEvery {
				continue
			}
			lastCleanup = now
			if err := st.Cleanup(); err != nil {
				log.Error("cleanup", "err", err)
			}
			if cfg.Backup.Keep > 0 && now.In(cfg.Location).Hour() >= backupHour {
				made, err := st.DailyBackup(ctx, cfg.Backup.Dir, cfg.Backup.Keep)
				if err != nil {
					log.Error("daily backup", "err", err)
				} else if made != "" {
					log.Info("daily backup written", "file", made)
				}
			}
		}
	}
}
