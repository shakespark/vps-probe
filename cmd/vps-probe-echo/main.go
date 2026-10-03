// Command vps-probe-echo answers vps-probe tunnel probes: an agent's `type:
// echo` peer sends authenticated requests, usually through a port forward,
// and this replies to valid ones. It is separate from the agent, which
// listens on no port; install it only on the node a tunnel ends at.
package main

import (
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
	fmt.Printf("ok: listen udp %s, answering sources: %s\n", cfg.Listen, sources(cfg))
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
	ctx, stop := cli.Context()
	defer stop()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	log.Info("echo responder started", "version", version, "listen", conn.LocalAddr(), "allow", sources(cfg))
	r := &echo.Responder{Key: []byte(cfg.Key), Allow: cfg.Prefixes, MaxPPS: cfg.MaxPPS, Log: log}
	if err := r.Serve(conn); err != nil {
		return err
	}
	log.Info("echo responder stopped")
	return nil
}
