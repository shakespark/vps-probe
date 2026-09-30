// Command vps-probe-agent collects metrics and pushes them to the server over
// encrypted UDP. It listens on no port and accepts no instructions.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"vpsprobe/internal/agent"
	"vpsprobe/internal/agent/config"
	"vpsprobe/internal/agent/report"
	pb "vpsprobe/internal/proto/probev1"
	"vpsprobe/internal/wire"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/vps-probe/agent.yml", "config file")
	dryRun := flag.Bool("dry-run", false, "print reports to stdout instead of sending them")
	debug := flag.Bool("debug", false, "debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	check := flag.Bool("check", false, "validate the config file and exit (safe next to a running agent)")
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

	if *check {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("ok: node %s, server %s, %d peers, state %s\n", cfg.Node, cfg.Server.Addr, len(cfg.Ping.Peers), cfg.StateDir)
		return
	}

	if err := run(*cfgPath, *dryRun, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, dryRun bool, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var sink agent.Sink
	if dryRun {
		p, err := newPrinter(cfg)
		if err != nil {
			return err
		}
		sink = p
	} else {
		s, err := report.NewSender(cfg.Server.Addr, cfg.Node, cfg.Server.Token, log)
		if err != nil {
			return err
		}
		go s.Run(ctx)
		sink = s
	}

	a, err := agent.New(cfg, sink, version, dryRun, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// printer shows each report as JSON plus the packet sizes it would produce.
type printer struct {
	cfg *config.Config
	enc protojson.MarshalOptions
}

func newPrinter(cfg *config.Config) (*printer, error) {
	return &printer{cfg: cfg, enc: protojson.MarshalOptions{Multiline: true, UseProtoNames: true}}, nil
}

func (p *printer) Enqueue(r *pb.Report) {
	aead, err := wire.NewAEAD(p.cfg.Server.Token, p.cfg.Node)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dry-run:", err)
		return
	}
	pkts, err := report.Packetize(aead, p.cfg.Node, proto.Clone(r).(*pb.Report))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dry-run: packetize:", err)
	}
	sizes := make([]int, len(pkts))
	for i, pk := range pkts {
		sizes[i] = len(pk.Bytes)
	}
	fmt.Printf("# report ts=%d packets=%v\n%s\n", r.Ts, sizes, p.enc.Format(r))
}
