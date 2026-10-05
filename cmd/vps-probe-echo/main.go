// Command vps-probe-echo answers vps-probe tunnel probes: an agent's `type:
// echo` peer sends authenticated requests, usually through a port forward,
// and this replies to valid ones. It is separate from the agent, which
// listens on no port; install it only on the node a tunnel ends at.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"

	"github.com/shakespark/vps-probe/internal/cli"
	"github.com/shakespark/vps-probe/internal/echo"
)

var version = "dev"

const usage = `usage:
  vps-probe-echo run [-config FILE] [-debug]     run the responder
  vps-probe-echo check [-config FILE]            validate the config and exit
  vps-probe-echo version
`

func main() {
	cli.Main(version, usage, cli.Command{Name: "run", Run: run}, cli.Command{Name: "check", Run: check})
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/vps-probe/echo.yml", "config file")
}

// listens names what the responder listens on.
func listens(cfg *echo.Config) string {
	if cfg.TCP {
		return "udp+tcp " + cfg.Listen
	}
	return "udp " + cfg.Listen
}

// sources names the addresses the responder answers.
func sources(cfg *echo.Config) string {
	if len(cfg.Prefixes) == 0 {
		return "any"
	}
	return strings.Trim(fmt.Sprint(cfg.Prefixes), "[]")
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := echo.LoadConfig(*path)
	if err != nil {
		return err
	}
	fmt.Printf("ok: listen %s, answering sources: %s\n", listens(cfg), sources(cfg))
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := configFlag(fs)
	debug := fs.Bool("debug", false, "debug logging")
	fs.Parse(args)
	log := cli.Logger(*debug)

	cfg, err := echo.LoadConfig(*path)
	if err != nil {
		return err
	}
	conn, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		return err
	}
	r := &echo.Responder{Key: []byte(cfg.Key), Allow: cfg.Prefixes, MaxPPS: cfg.MaxPPS, Log: log}
	tcpDone := make(chan error, 1)
	var ln net.Listener
	if cfg.TCP {
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			conn.Close()
			return err
		}
		go func() { tcpDone <- r.ServeTCP(ln) }()
	} else {
		tcpDone <- nil
	}
	ctx, stop := cli.Context()
	defer stop()
	go func() {
		<-ctx.Done()
		conn.Close()
		if ln != nil {
			ln.Close()
		}
	}()
	log.Info("echo responder started", "version", version, "listen", conn.LocalAddr(), "tcp", cfg.TCP, "allow", sources(cfg))
	if err := errors.Join(r.Serve(conn), <-tcpDone); err != nil {
		return err
	}
	log.Info("echo responder stopped")
	return nil
}
