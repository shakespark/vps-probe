// Command vps-probe-agent collects metrics and pushes them to the server over
// encrypted UDP. It listens on no port and accepts no instructions.
package main

import (
	"crypto/cipher"
	"flag"
	"fmt"
	"os"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/shakespark/vps-probe/internal/agent"
	"github.com/shakespark/vps-probe/internal/agent/config"
	"github.com/shakespark/vps-probe/internal/agent/report"
	"github.com/shakespark/vps-probe/internal/cli"
	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/wire"
)

var version = "dev"

const usage = `usage:
  vps-probe-agent [run] [-config FILE] [-debug] [-dry-run]   run the agent; -dry-run prints reports instead of
                                                             sending them and is safe next to a running agent
  vps-probe-agent check [-config FILE]                       validate the config and exit
  vps-probe-agent version
`

func main() {
	cli.Main(version, usage, cli.Command{Name: "run", Run: run}, cli.Command{Name: "check", Run: check})
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/vps-probe/agent.yml", "config file")
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("ok: node %s, server %s, %d peers\n", cfg.Node, cfg.Server, len(cfg.Peers))
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := configFlag(fs)
	debug := fs.Bool("debug", false, "debug logging")
	dryRun := fs.Bool("dry-run", false, "print reports to stdout instead of sending them")
	fs.Parse(args)
	log := cli.Logger(*debug)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := cli.Context()
	defer stop()

	var sink agent.Sink
	if *dryRun {
		aead, err := wire.NewAEAD(cfg.Token, cfg.Node)
		if err != nil {
			return err
		}
		sink = printer{cfg.Node, aead}
	} else {
		s, err := report.NewSender(cfg.Server, cfg.Node, cfg.Token, log)
		if err != nil {
			return err
		}
		go s.Run(ctx)
		sink = s
	}
	// systemd names the unit's StateDirectory in the environment; run by hand,
	// the agent uses the same place.
	a, err := agent.New(cfg, sink, agent.Options{Version: version, DryRun: *dryRun, StateDir: os.Getenv("STATE_DIRECTORY")}, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// printer shows each report as JSON plus the sizes of the packets it would
// be sent in.
type printer struct {
	node string
	aead cipher.AEAD
}

func (p printer) Enqueue(r *pb.Report) {
	pkts, err := report.Packetize(p.aead, p.node, proto.Clone(r).(*pb.Report))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dry-run: packetize:", err)
	}
	sizes := make([]int, len(pkts))
	for i, pk := range pkts {
		sizes[i] = len(pk.Bytes)
	}
	fmt.Printf("# report ts=%d packets=%v\n%s\n", r.Ts, sizes,
		protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Format(r))
}
