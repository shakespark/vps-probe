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

	"vpsprobe/internal/release"
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
  vps-probe-server test-notify [-config FILE]        send a test message to Telegram and every webhook
  vps-probe-server agent-config [-config FILE] -node ID -server HOST:PORT -o FILE
                                                     write agent.yml for a node (mode 0600)
  vps-probe-server add-node [-config FILE] -id ID [-name NAME] [-addr HOST] [-region R] [-group G]
                                                     add a node to the config, print its install command
  vps-probe-server install-cmd [-config FILE] -node ID
                                                     print the one-line command that installs a node's agent
  vps-probe-server install-cmd -upgrade              print a command that only upgrades an agent and keeps its
                                                     config; no token in it, the same for every node
                            add-node and install-cmd also take -server HOST:PORT (default: server_addr
                            in the config), -version V, -base URL and -signers FILE
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
	case "test-notify", "test-telegram":
		err = testNotify(args)
	case "agent-config":
		err = agentConfig(args)
	case "add-node":
		err = addNode(args)
	case "install-cmd":
		err = installCmd(args)
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

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func genToken() error {
	fmt.Println(newToken())
	return nil
}

// installFlags are shared by add-node and install-cmd: where agents report
// to and which release the command installs.
type installFlags struct {
	server, version, base, signers *string
}

func addInstallFlags(fs *flag.FlagSet) installFlags {
	return installFlags{
		server:  fs.String("server", "", "address agents use to reach this server, host:port (default: server_addr in the config)"),
		version: fs.String("version", "", "release the command installs (default: this server's version)"),
		base:    fs.String("base", "", "release base URL, e.g. a mirror (default: "+release.DefaultBase+")"),
		signers: fs.String("signers", "", "allowed_signers file to verify the release with (default: the built-in release keys)"),
	}
}

// line builds the install command for a node of cfg.
func (f installFlags) line(cfg *config.Config, node string) (string, error) {
	server := *f.server
	if server == "" {
		server = cfg.ServerAddr
	}
	if server == "" {
		return "", errors.New("pass -server HOST:PORT (how agents reach this server), or set server_addr in the config")
	}
	text, err := cfg.AgentConfig(node, server)
	if err != nil {
		return "", err
	}
	return f.command(release.Command{AgentConfig: text})
}

// command fills in the release to install and returns the line.
func (f installFlags) command(c release.Command) (string, error) {
	c.Version, c.Base = *f.version, *f.base
	if c.Version == "" {
		c.Version = version
	}
	if *f.signers != "" {
		b, err := os.ReadFile(*f.signers)
		if err != nil {
			return "", err
		}
		c.Signers = string(b)
	}
	return c.Line()
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install-cmd", flag.ExitOnError)
	path := configFlag(fs)
	node := fs.String("node", "", "node id from server.yml")
	upgrade := fs.Bool("upgrade", false, "print a command that only upgrades the agent and keeps the config on the VPS")
	f := addInstallFlags(fs)
	fs.Parse(args)
	if *upgrade {
		// Nothing node-specific: the config file is not even read.
		if *node != "" || *f.server != "" {
			return errors.New("install-cmd: -upgrade keeps each VPS's own agent.yml, so it takes no -node or -server; " +
				"to rewrite a node's config, run install-cmd -node ID without -upgrade")
		}
		line, err := f.command(release.Command{Upgrade: true})
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Run this as root on any VPS that already has the agent. It replaces the programs and keeps\n"+
			"/etc/vps-probe/agent.yml as it is. Or: vps-probe-server install-cmd -upgrade | ssh root@THAT_VPS sh\n\n")
		fmt.Println(line)
		return nil
	}
	if *node == "" {
		return errors.New("install-cmd: pass -node ID, or -upgrade")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	line, err := f.line(cfg, *node)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Run this as root on %s. It contains the node's token; to keep it out of every shell\n"+
		"history, pipe it instead: vps-probe-server install-cmd -node %s | ssh root@THAT_VPS sh\n\n", *node, *node)
	fmt.Println(line)
	return nil
}

// addNode inserts a node into the config file and prints what to do next.
// The file is replaced only if the result is a valid config; the previous
// version stays beside it as a .bak file.
func addNode(args []string) error {
	fs := flag.NewFlagSet("add-node", flag.ExitOnError)
	path := configFlag(fs)
	n := config.NewNode{}
	fs.StringVar(&n.ID, "id", "", "node id: letters, digits, - and _")
	fs.StringVar(&n.Name, "name", "", "display name (default: the id)")
	fs.StringVar(&n.Addr, "addr", "", "address other nodes ping, IP or domain (default: none, nobody pings it)")
	fs.StringVar(&n.Region, "region", "", "region code shown as a badge, e.g. HK")
	fs.StringVar(&n.Group, "group", "", "group for the overview tabs")
	f := addInstallFlags(fs)
	fs.Parse(args)
	if n.ID == "" {
		return errors.New("add-node: -id is required")
	}
	n.Token = newToken()

	// Load also checks the file's permissions.
	if _, err := config.Load(*path); err != nil {
		return err
	}
	old, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	data, err := config.AddNode(old, n)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	// Before touching the file: a command that cannot be printed (no
	// -server, a dev build without -version) should not leave a node behind.
	line, err := f.line(cfg, n.ID)
	if err != nil {
		return err
	}
	bak, err := replaceFile(*path, old, data)
	if err != nil {
		return err
	}

	w := os.Stderr
	fmt.Fprintf(w, "Added node %s to %s (previous version: %s).\n\n", n.ID, *path, bak)
	fmt.Fprintf(w, "1. Restart the server so it accepts the node:\n\n     systemctl restart vps-probe-server\n\n")
	fmt.Fprintf(w, "2. Right after that, run this as root on the new VPS. It contains the node's token; to keep\n"+
		"   it out of every shell history, pipe it instead:\n"+
		"     vps-probe-server install-cmd -node %s | ssh root@THAT_VPS sh\n\n", n.ID)
	fmt.Println(line)
	var others []string
	for _, p := range cfg.Nodes {
		if p.ID != n.ID && n.Addr != "" && !cfg.NoPing(n.ID, p.ID) {
			others = append(others, p.ID)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(w, "\n3. Optional: for the other nodes to ping %s, reinstall their config. For each of\n   %s:\n\n"+
			"     vps-probe-server install-cmd -node ID     # run the printed command on that VPS\n",
			n.ID, strings.Join(others, ", "))
	}
	return nil
}

// replaceFile swaps path's content for data, keeping owner and mode, after
// saving the old content beside it. It returns the backup's name.
func replaceFile(path string, old, data []byte) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	uid, gid := -1, -1
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(sys.Uid), int(sys.Gid)
	}
	write := func(name string, b []byte) error {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Chmod(st.Mode().Perm()) // the umask may have narrowed it
		}
		if err == nil && uid >= 0 {
			err = f.Chown(uid, gid)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
		}
		return err
	}
	bak := path + ".bak-" + time.Now().Format("20060102-150405")
	if err := write(bak, old); err != nil {
		return "", err
	}
	tmp := path + ".new"
	os.Remove(tmp)
	if err := write(tmp, data); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return bak, nil
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

// testNotify sends one message to every configured channel and reports
// each result; a failure here is not retried.
func testNotify(args []string) error {
	fs := flag.NewFlagSet("test-notify", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if len(cfg.Channels()) == 0 {
		return errors.New("no channel is configured (set telegram.bot_token and telegram.chat_id, or add a webhook)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg := fmt.Sprintf("✅ vps-probe 测试消息\n%d 个节点，%d 条告警规则\n%s", len(cfg.Nodes), len(cfg.Alerts),
		time.Now().In(cfg.Location).Format("2006-01-02 15:04:05"))
	failed := 0
	report := func(name string, err error) {
		if err != nil {
			failed++
			fmt.Printf("%s: FAILED: %v\n", name, err)
		} else {
			fmt.Printf("%s: sent\n", name)
		}
	}
	if cfg.Telegram.Enabled() {
		report("telegram", notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, slog.Default()).Send(ctx, msg))
	}
	for _, w := range cfg.Webhooks {
		report(w.Name, notify.NewWebhook(w.Name, w.Method, w.URL, w.Headers, w.Body, slog.Default()).Send(ctx, msg))
	}
	if failed > 0 {
		return fmt.Errorf("%d channel(s) failed", failed)
	}
	return nil
}

func agentConfig(args []string) error {
	fs := flag.NewFlagSet("agent-config", flag.ExitOnError)
	path := configFlag(fs)
	node := fs.String("node", "", "node id from server.yml")
	server := fs.String("server", "", "address agents use to reach this server, host:port (default: server_addr in the config)")
	out := fs.String("o", "", "output file, written with mode 0600 (\"-\" for stdout)")
	fs.Parse(args)
	if *node == "" || *out == "" {
		return errors.New("agent-config: -node and -o are required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *server == "" {
		*server = cfg.ServerAddr
	}
	if *server == "" {
		return errors.New("agent-config: pass -server HOST:PORT, or set server_addr in the config")
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
	// Channels: Telegram and any webhooks; with none, alerts are only
	// recorded and logged. Each has its own queue and sender goroutine.
	var channels notify.Multi
	var senders []func(context.Context)
	if cfg.Telegram.Enabled() {
		tg := notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, log)
		channels, senders = append(channels, tg), append(senders, tg.Run)
	}
	for _, w := range cfg.Webhooks {
		wh := notify.NewWebhook(w.Name, w.Method, w.URL, w.Headers, w.Body, log)
		channels, senders = append(channels, wh), append(senders, wh.Run)
	}
	var notifier notify.Notifier = channels
	if len(channels) == 0 {
		notifier = notify.Log{Log: log}
		log.Warn("no telegram or webhook configured: alerts are recorded and logged but not sent")
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
	for _, run := range senders {
		go run(ctx) // not waited for: an in-flight HTTP request must not delay shutdown
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
		"nodes", len(cfg.Nodes), "db", cfg.DB, "alert_rules", len(cfg.Alerts), "notify", strings.Join(cfg.Channels(), ","), "cf_access", cfg.CFAccess.Enabled(), "basic_auth", cfg.BasicAuth.Enabled())

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
