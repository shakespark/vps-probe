// Command vps-probe-server receives agent reports over encrypted UDP, keeps
// everything in one SQLite file and serves a read-only web API.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"vpsprobe/internal/server/alert"
	"vpsprobe/internal/server/api"
	"vpsprobe/internal/server/basicauth"
	"vpsprobe/internal/server/cfaccess"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/server/ingest"
	"vpsprobe/internal/server/notify"
	"vpsprobe/internal/server/store"
	"vpsprobe/web"
)

var version = "dev"

const (
	rollupEvery   = time.Minute
	rollupWindow  = 3 * time.Hour // agents deliver backlog up to 2h late
	cleanupEvery  = time.Hour
	backupHour    = 4 // local time; the daily backup runs after this hour
	shutdownGrace = 10 * time.Second
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  vps-probe-server [serve] [-config FILE] [-debug]   run the server
  vps-probe-server check [-config FILE]              validate the config and exit
  vps-probe-server backup [-config FILE] -o FILE     write a consistent copy of the database
  vps-probe-server test-telegram [-config FILE]      send a test message with the configured bot
  vps-probe-server agent-config [-config FILE] -node ID -server HOST:PORT -o FILE
                                                     write agent.yml for a node (mode 0600)
  vps-probe-server gen-token                         print a new random node token
  vps-probe-server hash-password                     read a password, print its hash for basic_auth
  vps-probe-server version
`)
}

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "check":
		err = check(args)
	case "backup":
		err = backup(args)
	case "test-telegram":
		err = testTelegram(args)
	case "agent-config":
		err = agentConfig(args)
	case "gen-token":
		err = genToken()
	case "hash-password":
		err = hashPassword()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/vps-probe/server.yml", "config file")
}

func genToken() error {
	b := make([]byte, 32)
	rand.Read(b)
	fmt.Println(base64.RawURLEncoding.EncodeToString(b))
	return nil
}

// hashPassword prints the bcrypt hash for basic_auth.password_hash. On a
// terminal it asks twice without echo; otherwise it reads one line from
// stdin, so the password never has to appear in the shell history.
func hashPassword() error {
	var pw string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password: ")
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Again: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return errors.New("the two passwords differ")
		}
		pw = string(a)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read the password from stdin: %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	// bcrypt ignores everything after 72 bytes; refuse rather than truncate.
	if len(pw) < 10 || len(pw) > 72 {
		return errors.New("want a password of 10 to 72 bytes")
	}
	h, err := basicauth.Hash(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %d nodes, ingest udp %s, web %s, db %s\n", len(cfg.Nodes), cfg.Listen.Ingest, cfg.Listen.Web, cfg.DB)
	return nil
}

func testTelegram(args []string) error {
	fs := flag.NewFlagSet("test-telegram", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if !cfg.Telegram.Enabled() {
		return errors.New("telegram is not configured (set telegram.bot_token and telegram.chat_id)")
	}
	tg := notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg := fmt.Sprintf("✅ vps-probe 测试消息\n%d 个节点，%d 条告警规则\n%s", len(cfg.Nodes), len(cfg.Alerts),
		time.Now().In(cfg.Location).Format("2006-01-02 15:04:05"))
	if err := tg.Send(ctx, msg); err != nil {
		return err
	}
	fmt.Println("sent")
	return nil
}

func agentConfig(args []string) error {
	fs := flag.NewFlagSet("agent-config", flag.ExitOnError)
	path := configFlag(fs)
	node := fs.String("node", "", "node id from server.yml")
	server := fs.String("server", "", "address agents use to reach this server, host:port (e.g. 203.0.113.1:9527)")
	out := fs.String("o", "", "output file, written with mode 0600 (\"-\" for stdout)")
	fs.Parse(args)
	if *node == "" || *server == "" || *out == "" {
		return errors.New("agent-config: -node, -server and -o are required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	text, err := cfg.AgentConfig(*node, *server)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = os.Stdout.WriteString(text)
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // an existing file keeps its mode otherwise
		f.Close()
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (contains the token for %s; copy it to that VPS and delete it here)\n", *out, *node)
	return nil
}

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	path := configFlag(fs)
	out := fs.String("o", "", "output file (must not exist)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("backup: -o is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := store.Backup(context.Background(), cfg.DB, *out); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	debug := fs.Bool("debug", false, "debug logging")
	fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ret := store.Retention{
		Raw: time.Duration(cfg.Retention.Raw),
		M5:  time.Duration(cfg.Retention.M5),
		H1:  time.Duration(cfg.Retention.H1),
	}
	st, err := store.Open(cfg.DB, store.Options{Location: cfg.Location, Retention: ret, Log: log})
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
	ids := make([]string, len(cfg.Nodes))
	for i, n := range cfg.Nodes {
		ids[i] = n.ID
	}
	if err := st.SyncNodes(ids); err != nil {
		return err
	}
	// Catch up on buckets left open by a previous shutdown or crash.
	if err := st.Rollup(time.Now().Add(-ret.Raw)); err != nil {
		return err
	}
	if err := st.Cleanup(ret); err != nil {
		return err
	}

	in, err := ingest.Listen(cfg.Listen.Ingest, cfg.Nodes, st, log)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	var notifier notify.Notifier = notify.Log{Log: log}
	var tg *notify.Telegram
	if cfg.Telegram.Enabled() {
		tg = notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, log)
		notifier = tg
	} else {
		log.Warn("telegram not configured: alerts are recorded and logged but not sent")
	}
	ev := alert.New(cfg, st, notifier, log)
	if err := ev.Load(); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen.Web)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	if host, _, _ := net.SplitHostPort(cfg.Listen.Web); !isLoopback(host) {
		switch {
		case cfg.BasicAuth.Enabled():
			log.Warn("web UI is not bound to loopback: basic_auth sends the password with every request, serve it only through an HTTPS reverse proxy",
				"listen", cfg.Listen.Web)
		case !cfg.CFAccess.Enabled():
			log.Warn("web UI is not bound to loopback and neither cf_access nor basic_auth is set: anyone who can reach it sees everything",
				"listen", cfg.Listen.Web)
		}
	}
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); ev.Run(ctx) }()
	if tg != nil {
		go tg.Run(ctx) // not waited for: an in-flight HTTP request must not delay shutdown
	}
	if access != nil {
		go access.Run(ctx)
	}
	go func() { defer wg.Done(); in.Run(ctx) }()
	go func() { defer wg.Done(); maintain(ctx, cfg, st, ret, log) }()
	go func() {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web", "err", err)
			stop()
		}
	}()
	log.Info("server started", "version", version, "ingest", in.Addr(), "web", ln.Addr(),
		"nodes", len(cfg.Nodes), "db", cfg.DB, "alert_rules", len(cfg.Alerts), "telegram", cfg.Telegram.Enabled(), "cf_access", cfg.CFAccess.Enabled(), "basic_auth", cfg.BasicAuth.Enabled())

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	srv.Shutdown(sctx)
	wg.Wait() // no writer may be active when the database closes
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maintain runs rollups, cleanup and the daily backup. It shares the single
// writer connection with ingest, so each step briefly delays report writes.
func maintain(ctx context.Context, cfg *config.Config, st *store.Store, ret store.Retention, log *slog.Logger) {
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
			if err := st.Rollup(now.Add(-rollupWindow)); err != nil {
				log.Error("rollup", "err", err)
			}
			if now.Sub(lastCleanup) < cleanupEvery {
				continue
			}
			lastCleanup = now
			if err := st.Cleanup(ret); err != nil {
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
