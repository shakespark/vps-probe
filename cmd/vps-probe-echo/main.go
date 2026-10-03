// Command vps-probe-echo answers vps-probe tunnel probes: an agent's `type:
// echo` peer sends authenticated requests, usually through a port forward,
// and this replies to valid ones. It is separate from the agent, which
// listens on no port; install it only on the node a tunnel ends at.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/shakespark/vps-probe/internal/echo"
)

var version = "dev"

type config struct {
	Listen string   `yaml:"listen"`
	Key    string   `yaml:"key"`
	Allow  []string `yaml:"allow"` // source IPs or CIDRs; empty = any
	MaxPPS int      `yaml:"max_pps"`

	allow []netip.Prefix
}

func load(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &config{Listen: ":39527"}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var errs []error
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("listen %q: want [host]:port", c.Listen))
	}
	if len(c.Key) < echo.MinKeyLen {
		errs = append(errs, fmt.Errorf("key: must be at least %d characters (use vps-probe-server gen-token)", echo.MinKeyLen))
	}
	for _, a := range c.Allow {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			ip, err2 := netip.ParseAddr(a)
			if err2 != nil {
				errs = append(errs, fmt.Errorf("allow: %q is not an IP or CIDR", a))
				continue
			}
			p = netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen())
		}
		c.allow = append(c.allow, p.Masked())
	}
	if c.MaxPPS < 0 {
		errs = append(errs, errors.New("max_pps: must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

func main() {
	cfgPath := flag.String("config", "/etc/vps-probe/echo.yml", "config file")
	check := flag.Bool("check", false, "validate the config file and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *check {
		allow := "any source"
		if len(cfg.allow) > 0 {
			allow = fmt.Sprint(cfg.allow)
		}
		fmt.Printf("ok: listen udp %s, answering %s\n", cfg.Listen, allow)
		return
	}

	conn, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	allow := "any"
	if len(cfg.allow) > 0 {
		allow = strings.Trim(fmt.Sprint(cfg.allow), "[]")
	}
	log.Info("echo responder started", "version", version, "listen", conn.LocalAddr(), "allow", allow)
	r := &echo.Responder{Key: []byte(cfg.Key), Allow: cfg.allow, MaxPPS: cfg.MaxPPS, Log: log}
	if err := r.Serve(conn); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	log.Info("echo responder stopped")
}
