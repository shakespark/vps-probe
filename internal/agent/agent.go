// Package agent is the sampling loop: every interval it gathers what the
// collectors, the traffic accountant and the pinger have, and hands one
// report to the sink.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shakespark/vps-probe/internal/agent/collect"
	"github.com/shakespark/vps-probe/internal/agent/config"
	"github.com/shakespark/vps-probe/internal/agent/ping"
	"github.com/shakespark/vps-probe/internal/agent/traffic"
	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/wire"
)

const (
	// StateDir holds the traffic totals. It is the unit's StateDirectory,
	// the only place the service may write.
	StateDir = "/var/lib/vps-probe-agent"

	diskEvery   = time.Minute
	sysEvery    = time.Hour
	stateFile   = "traffic.json"
	shutdownMax = 5 * time.Second
)

// Sink receives finished reports: the UDP sender, or stdout in a dry run.
type Sink interface {
	Enqueue(*pb.Report)
}

// Options are what a caller may vary; the zero value is the installed agent.
type Options struct {
	Version string
	// DryRun loads the traffic state read-only and never saves it, so a dry
	// run can't disturb the installed agent.
	DryRun bool
	// StateDir and FS default to StateDir and the real filesystem.
	StateDir string
	FS       collect.FS
}

type Agent struct {
	cfg     *config.Config
	log     *slog.Logger
	sink    Sink
	version string

	fs      collect.FS
	sampler *collect.Sampler
	ifaces  *collect.Selector
	acct    *traffic.Accountant
	pinger  *ping.Pinger // nil without peers
	lock    *os.File     // held for the process lifetime; nil in a dry run
	sources sources

	bootID   string
	bootTime time.Time // zero when unknown

	lastDisk time.Time
	lastSys  time.Time
}

func New(cfg *config.Config, sink Sink, opt Options, log *slog.Logger) (*Agent, error) {
	if opt.StateDir == "" {
		opt.StateDir = StateDir
	}
	if opt.FS == (collect.FS{}) {
		opt.FS = collect.Host
	}
	a := &Agent{cfg: cfg, log: log, sink: sink, version: opt.Version, fs: opt.FS, sources: sources{log: log}}

	// The boot id is required: without it a reboot cannot be told from a
	// counter that kept running. The boot time is not. Where it cannot be
	// read (a container whose lxcfs has died) it stays zero, which traffic
	// accounting takes as unknown.
	var err error
	if a.bootID, err = a.fs.BootID(); err != nil {
		return nil, err
	}
	if a.bootTime, err = a.fs.SystemStart(time.Now()); err != nil {
		a.bootTime = time.Time{}
		log.Warn("boot time unknown: starting without it", "err", err)
	}

	statePath := filepath.Join(opt.StateDir, stateFile)
	if opt.DryRun {
		a.acct, err = traffic.OpenReadOnly(statePath, cfg.Traffic.Location, cfg.Traffic.Reset, log)
	} else {
		// Two writers would overwrite each other's increments.
		if a.lock, err = lockFile(statePath + ".lock"); err != nil {
			return nil, err
		}
		a.acct, err = traffic.Open(statePath, cfg.Traffic.Location, cfg.Traffic.Reset, log)
	}
	if err != nil {
		return nil, err
	}
	if len(cfg.Peers) > 0 {
		// Latency is one feature among several; run without it rather than
		// not at all.
		if a.pinger, err = ping.New(cfg.Peers, ping.Interval, ping.Timeout, log); err != nil {
			log.Error("ping disabled", "err", err)
		}
	}
	a.ifaces = collect.NewSelector(a.fs, cfg.Interfaces, log)
	a.sampler = collect.NewSampler(a.fs)
	return a, nil
}

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another agent instance holds %s (use -dry-run to test alongside it)", path)
		}
		return nil, err
	}
	return f, nil
}

// Run samples on interval boundaries until ctx is done, then records final
// traffic counters and saves state, so a clean shutdown loses nothing.
func (a *Agent) Run(ctx context.Context) error {
	if a.pinger != nil {
		go a.pinger.Run(ctx)
	}
	r := a.cfg.Traffic.Reset
	a.log.Info("agent started", "version", a.version, "node", a.cfg.Node, "server", a.cfg.Server,
		"peers", len(a.cfg.Peers), "period_reset", fmt.Sprintf("day %d %02d:%02d %s", r.Day, r.Hour, r.Minute, a.cfg.Traffic.Location))
	for {
		now := time.Now()
		next := now.Truncate(wire.Interval).Add(wire.Interval)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return a.shutdown()
		case <-timer.C:
		}
		a.sink.Enqueue(a.sample(next, time.Now()))
	}
}

func (a *Agent) shutdown() error {
	done := make(chan error, 1)
	go func() {
		now := time.Now()
		a.network(now, a.ifaces.Interfaces(now), &pb.Report{})
		done <- a.acct.Save()
	}()
	select {
	case err := <-done:
		if err != nil {
			a.log.Error("final traffic save failed", "err", err)
			return err
		}
		if a.lock == nil {
			a.log.Info("dry run: traffic state not saved, exiting")
		} else {
			a.log.Info("traffic state saved, exiting")
		}
		return nil
	case <-time.After(shutdownMax):
		a.log.Error("final traffic save timed out")
		return context.DeadlineExceeded
	}
}

// sample builds the report for the interval ending at ts. A source that
// cannot be read is left out of the report; the rest is sent.
func (a *Agent) sample(ts, now time.Time) *pb.Report {
	rep := &pb.Report{Ts: ts.Unix()}
	ifaces := a.ifaces.Interfaces(now)
	a.network(now, ifaces, rep)
	for _, t := range a.acct.Snapshot(now, ifaces) {
		rep.Traffic = append(rep.Traffic, &pb.IfaceTraffic{Iface: t.Iface, Cur: period(t.Cur), Prev: period(t.Prev)})
	}

	if c, ok, err := a.sampler.CPU(); a.sources.ok("cpu", err) && ok {
		rep.Cpu = &pb.CPU{Usage: float32(c.Usage), Steal: float32(c.Steal), Softirq: float32(c.SoftIRQ)}
	}
	if l, err := a.fs.Load(); a.sources.ok("load", err) {
		rep.Load = &pb.Load{L1: float32(l.L1), L5: float32(l.L5), L15: float32(l.L15), Threads: l.Threads}
	}
	if m, err := a.fs.Mem(); a.sources.ok("memory", err) {
		rep.Mem = &pb.Mem{Total: m.Total, Used: m.Used, SwapTotal: m.SwapTotal, SwapUsed: m.SwapUsed}
	}
	if k, err := a.fs.Sockets(); a.sources.ok("sockets", err) {
		rep.Sockets = &pb.Sockets{Tcp: k.TCP, Udp: k.UDP, TcpTw: k.TCPTimeWait}
	}

	if now.Sub(a.lastDisk) >= diskEvery {
		a.lastDisk = now
		for _, mnt := range a.cfg.Disks {
			if d, err := a.fs.Disk(mnt); a.sources.ok("disk "+mnt, err) {
				rep.Disks = append(rep.Disks, &pb.Disk{Mount: d.Mount, Total: d.Total, Used: d.Used,
					Avail: d.Avail, InodePct: float32(d.InodePct)})
			}
		}
	}
	if now.Sub(a.lastSys) >= sysEvery {
		a.lastSys = now
		s := a.fs.SysInfo()
		var boot int64 // 0 = unknown; the zero time's Unix() is not 0
		if !s.BootTime.IsZero() {
			boot = s.BootTime.Unix()
		}
		rep.Sys = &pb.SysInfo{Hostname: s.Hostname, Os: s.OS, Kernel: s.Kernel, Arch: s.Arch,
			Cores: uint32(s.Cores), BootTime: boot, AgentVersion: a.version}
	}
	if a.pinger != nil {
		for _, s := range a.pinger.Snapshot(now) {
			rep.Pings = append(rep.Pings, &pb.Ping{Target: s.Target, Addr: s.Addr,
				Sent: uint32(s.Sent), Lost: uint32(s.Lost),
				Min: float32(s.Min), Avg: float32(s.Avg), Max: float32(s.Max), Jitter: float32(s.Jitter)})
		}
	}
	return rep
}

// network reads the interface counters once, for both of their uses: the
// counted interfaces' bytes go into the traffic totals, and their speeds
// into rep.
func (a *Agent) network(now time.Time, ifaces []string, rep *pb.Report) {
	counters, rates, err := a.sampler.Net(now)
	if !a.sources.ok("network counters", err) {
		return
	}
	counted := make(map[string]traffic.Counter, len(ifaces))
	for _, name := range ifaces {
		if c, ok := counters[name]; ok {
			counted[name] = traffic.Counter{RX: c.RX, TX: c.TX}
		}
		if r, ok := rates[name]; ok {
			rep.Net = append(rep.Net, &pb.NetRate{Iface: name, RxRate: r.RX, TxRate: r.TX, RxPps: r.RXPkts, TxPps: r.TXPkts})
		}
	}
	a.acct.Update(traffic.Sample{Time: now, BootID: a.bootID, BootTime: a.bootTime, Counters: counted})
	if err := a.acct.MaybeSave(now); err != nil {
		a.log.Error("traffic save", "err", err)
	}
}

func period(p traffic.Period) *pb.Period {
	return &pb.Period{Start: p.Start.Unix(), End: p.End.Unix(), Rx: p.RX, Tx: p.TX}
}

// sources remembers which sources could not be read, so that one that stays
// unreadable (the files lxcfs provides, once lxcfs has died) is logged when
// it fails and when it comes back, not at every sample.
type sources struct {
	log     *slog.Logger
	failing map[string]bool
}

// ok reports whether err is nil, logging a change in either direction.
func (s *sources) ok(what string, err error) bool {
	switch {
	case err != nil && !s.failing[what]:
		if s.failing == nil {
			s.failing = map[string]bool{}
		}
		s.failing[what] = true
		s.log.Error("cannot read "+what+": not reported until it can be read again", "err", err)
	case err == nil && s.failing[what]:
		delete(s.failing, what)
		s.log.Info(what + " can be read again")
	}
	return err == nil
}
