// Package agent wires collectors, traffic accounting, ping and reporting
// together into the sampling loop.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"vpsprobe/internal/agent/collect"
	"vpsprobe/internal/agent/config"
	"vpsprobe/internal/agent/ping"
	"vpsprobe/internal/agent/traffic"
	pb "vpsprobe/internal/proto/probev1"
)

const (
	diskEvery   = time.Minute
	sysEvery    = time.Hour
	ifaceEvery  = time.Minute
	stateFile   = "traffic.json"
	shutdownMax = 5 * time.Second
)

// Sink receives finished reports: the UDP sender, or stdout in dry-run mode.
type Sink interface {
	Enqueue(*pb.Report)
}

type Agent struct {
	cfg     *config.Config
	log     *slog.Logger
	fs      collect.FS
	sink    Sink
	acct    *traffic.Accountant
	pinger  *ping.Pinger
	version string
	lock    *os.File // held for the process lifetime; nil in dry-run

	bootID   string
	bootTime time.Time

	prevCPU   collect.CPUTimes
	havePrevC bool
	prevNet   map[string]collect.NetCounter
	prevNetAt time.Time

	ifaces   []string
	ifacesAt time.Time
	lastDisk time.Time
	lastSys  time.Time
}

// New prepares an agent. In dryRun mode the traffic state is loaded
// read-only and never saved, so a dry run can't disturb the installed agent.
func New(cfg *config.Config, sink Sink, version string, dryRun bool, log *slog.Logger) (*Agent, error) {
	a := &Agent{cfg: cfg, log: log, fs: collect.Host, sink: sink, version: version}

	var err error
	if a.bootID, err = a.fs.BootID(); err != nil {
		return nil, err
	}
	if a.bootTime, err = a.fs.BootTime(); err != nil {
		return nil, err
	}
	statePath := filepath.Join(cfg.StateDir, stateFile)
	reset := traffic.Reset{Day: cfg.Traffic.ResetDay, Hour: cfg.Traffic.ResetHour, Minute: cfg.Traffic.ResetMinute}
	if dryRun {
		a.acct, err = traffic.OpenReadOnly(statePath, cfg.Traffic.Location, reset, log)
	} else {
		// Two writers would overwrite each other's increments.
		if a.lock, err = lockFile(statePath + ".lock"); err != nil {
			return nil, err
		}
		a.acct, err = traffic.Open(statePath, cfg.Traffic.Location, reset, log)
	}
	if err != nil {
		return nil, err
	}
	if len(cfg.Ping.Peers) > 0 {
		peers := make([]ping.Peer, len(cfg.Ping.Peers))
		for i, p := range cfg.Ping.Peers {
			typ := p.Type
			if typ == "icmp" {
				typ = ping.TypeICMP
			}
			peers[i] = ping.Peer{Name: p.Name, Addr: p.Addr, Type: typ, Key: p.Key}
		}
		// Latency is one feature among several; run without it rather than
		// not at all.
		if a.pinger, err = ping.New(peers, cfg.Ping.Interval, cfg.Ping.Timeout, log); err != nil {
			log.Error("ping disabled", "err", err)
		}
	}
	if err := a.refreshIfaces(time.Now()); err != nil {
		return nil, err
	}
	return a, nil
}

// Run samples on interval boundaries until ctx is done, then records final
// traffic counters and saves state, so a clean shutdown loses nothing.
func (a *Agent) Run(ctx context.Context) error {
	if a.pinger != nil {
		go a.pinger.Run(ctx)
	}
	// Baselines so the first report has CPU usage and rates.
	if c, err := a.fs.ReadCPU(); err == nil {
		a.prevCPU, a.havePrevC = c, true
	}
	if n, err := a.fs.ReadNetDev(); err == nil {
		a.prevNet, a.prevNetAt = n, time.Now()
	}
	a.log.Info("agent started", "node", a.cfg.Node, "server", a.cfg.Server.Addr,
		"interval", a.cfg.Interval, "interfaces", a.ifaces, "peers", len(a.cfg.Ping.Peers),
		"period_reset_day", a.cfg.Traffic.ResetDay,
		"period_reset_time", fmt.Sprintf("%02d:%02d", a.cfg.Traffic.ResetHour, a.cfg.Traffic.ResetMinute), "timezone", a.cfg.Traffic.Location)

	for {
		now := time.Now()
		next := now.Truncate(a.cfg.Interval).Add(a.cfg.Interval)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return a.shutdown()
		case <-timer.C:
		}
		a.tick(next)
	}
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

func (a *Agent) shutdown() error {
	done := make(chan error, 1)
	go func() {
		if n, err := a.fs.ReadNetDev(); err == nil {
			a.updateTraffic(time.Now(), n)
		}
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

func (a *Agent) refreshIfaces(now time.Time) error {
	if len(a.cfg.Interfaces) > 0 {
		a.ifaces = a.cfg.Interfaces
		return nil
	}
	if !a.ifacesAt.IsZero() && now.Sub(a.ifacesAt) < ifaceEvery {
		return nil
	}
	a.ifacesAt = now
	found, err := a.fs.DetectInterfaces()
	if err != nil {
		return err
	}
	if a.ifaces != nil && !slices.Equal(found, a.ifaces) {
		a.log.Warn("physical interfaces changed", "from", a.ifaces, "to", found)
	}
	a.ifaces = found
	return nil
}

func (a *Agent) updateTraffic(now time.Time, all map[string]collect.NetCounter) {
	counters := make(map[string]traffic.Counter, len(a.ifaces))
	for _, name := range a.ifaces {
		if c, ok := all[name]; ok {
			counters[name] = traffic.Counter{RX: c.RX, TX: c.TX}
		}
	}
	err := a.acct.Update(traffic.Sample{Time: now, BootID: a.bootID, BootTime: a.bootTime, Counters: counters})
	if err != nil {
		a.log.Error("traffic update", "err", err)
	}
}

func (a *Agent) tick(ts time.Time) {
	now := time.Now()
	rep := &pb.Report{Ts: ts.Unix()}

	if err := a.refreshIfaces(now); err != nil {
		a.log.Warn("interface detection", "err", err) // keep the previous list
	}

	if netNow, err := a.fs.ReadNetDev(); err != nil {
		a.log.Error("read /proc/net/dev", "err", err)
	} else {
		a.updateTraffic(now, netNow)
		if err := a.acct.MaybeSave(now); err != nil {
			a.log.Error("traffic save", "err", err)
		}
		if a.prevNet != nil {
			secs := now.Sub(a.prevNetAt).Seconds()
			for _, name := range a.ifaces {
				cur, ok1 := netNow[name]
				prev, ok2 := a.prevNet[name]
				if !ok1 || !ok2 || secs <= 0 || cur.RX < prev.RX || cur.TX < prev.TX ||
					cur.RXPkts < prev.RXPkts || cur.TXPkts < prev.TXPkts {
					continue
				}
				per := func(c, p uint64) uint64 { return uint64(float64(c-p) / secs) }
				rxPPS, txPPS := per(cur.RXPkts, prev.RXPkts), per(cur.TXPkts, prev.TXPkts)
				rep.Net = append(rep.Net, &pb.NetRate{
					Iface:  name,
					RxRate: per(cur.RX, prev.RX),
					TxRate: per(cur.TX, prev.TX),
					RxPps:  &rxPPS,
					TxPps:  &txPPS,
				})
			}
		}
		a.prevNet, a.prevNetAt = netNow, now
	}
	for _, t := range a.acct.Snapshot(now, a.ifaces) {
		rep.Traffic = append(rep.Traffic, &pb.IfaceTraffic{
			Iface: t.Iface,
			Cur:   &pb.Period{Start: t.CurStart, Rx: t.Cur.RX, Tx: t.Cur.TX},
			Prev:  &pb.Period{Start: t.PrevStart, Rx: t.Prev.RX, Tx: t.Prev.TX},
		})
	}

	if c, err := a.fs.ReadCPU(); err != nil {
		a.log.Error("read cpu", "err", err)
	} else {
		if a.havePrevC {
			if p, ok := collect.CPUUsage(a.prevCPU, c); ok {
				softirq := float32(p.SoftIRQ)
				rep.Cpu = &pb.CPU{Usage: float32(p.Usage), Steal: float32(p.Steal), Softirq: &softirq}
			}
		}
		a.prevCPU, a.havePrevC = c, true
	}
	if l, err := a.fs.ReadLoad(); err != nil {
		a.log.Error("read load", "err", err)
	} else {
		rep.Load = &pb.Load{L1: float32(l.L1), L5: float32(l.L5), L15: float32(l.L15), Threads: l.Threads}
	}
	if m, err := a.fs.ReadMem(); err != nil {
		a.log.Error("read memory", "err", err)
	} else {
		rep.Mem = &pb.Mem{Total: m.Total, Used: m.Used, SwapTotal: m.SwapTotal, SwapUsed: m.SwapUsed}
	}
	if k, err := a.fs.ReadSockets(); err != nil {
		a.log.Error("read sockets", "err", err)
	} else {
		rep.Sockets = &pb.Sockets{Tcp: k.TCP, Udp: k.UDP, TcpTw: k.TCPTimeWait}
	}

	if a.lastDisk.IsZero() || now.Sub(a.lastDisk) >= diskEvery {
		a.lastDisk = now
		for _, mnt := range a.cfg.Disks {
			d, err := collect.DiskUsage(mnt)
			if err != nil {
				a.log.Warn("disk usage", "mount", mnt, "err", err)
				continue
			}
			rep.Disks = append(rep.Disks, &pb.Disk{Mount: d.Mount, Total: d.Total, Used: d.Used,
				Avail: d.Avail, InodePct: float32(d.InodePct)})
		}
	}

	if a.lastSys.IsZero() || now.Sub(a.lastSys) >= sysEvery {
		a.lastSys = now
		s := a.fs.ReadSysInfo()
		rep.Sys = &pb.SysInfo{Hostname: s.Hostname, Os: s.OS, Kernel: s.Kernel, Arch: s.Arch,
			Cores: uint32(s.Cores), BootTime: s.BootTime.Unix(), Uptime: s.Uptime, AgentVersion: a.version}
	}

	if a.pinger != nil {
		for _, s := range a.pinger.Snapshot(now) {
			rep.Pings = append(rep.Pings, &pb.Ping{Target: s.Target, Addr: s.Addr,
				Sent: uint32(s.Sent), Lost: uint32(s.Lost),
				Min: float32(s.Min), Avg: float32(s.Avg), Max: float32(s.Max), Jitter: float32(s.Jitter)})
		}
	}

	a.sink.Enqueue(rep)
}
