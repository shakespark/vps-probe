// Package traffic accumulates per-interface byte counters into billing
// periods and persists them, so totals survive agent restarts and reboots.
//
// The kernel counters in /proc/net/dev restart from zero on every boot. Each
// interface remembers the boot it last saw and its last reading:
//
//   - same boot, counter grew:       delta = cur - last
//   - same boot, counter went down:  delta = cur (driver reload / iface recreated)
//   - different boot:                delta = cur (everything since boot is new)
//   - interface never seen before:   delta = cur if the machine booted inside
//     the current period, otherwise 0 (we can't tell which period the bytes
//     since boot belong to, so we only take a baseline)
//
// The agent's state file is the source of truth; the server holds a copy.
package traffic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	stateVersion = 1
	// KeepPeriods is how many periods are retained per interface.
	KeepPeriods = 24
	// SaveEvery bounds how much traffic a power loss can lose.
	SaveEvery = 30 * time.Second
)

// Counter is a raw kernel byte counter pair.
type Counter struct {
	RX, TX uint64
}

// Sample is one reading of all monitored interfaces.
type Sample struct {
	Time     time.Time
	BootID   string
	BootTime time.Time
	Counters map[string]Counter
}

// Totals is the traffic accumulated in one period.
type Totals struct {
	RX uint64 `json:"rx"`
	TX uint64 `json:"tx"`
}

// IfaceTotals is what gets reported for one interface.
type IfaceTotals struct {
	Iface     string
	CurStart  string
	Cur       Totals
	PrevStart string
	Prev      Totals
}

type ifaceState struct {
	BootID  string             `json:"boot_id"`
	LastRX  uint64             `json:"last_rx"`
	LastTX  uint64             `json:"last_tx"`
	Periods map[string]*Totals `json:"periods"`
}

type state struct {
	Version    int                    `json:"version"`
	Interfaces map[string]*ifaceState `json:"interfaces"`
}

// Accountant is safe for concurrent use.
type Accountant struct {
	path     string
	loc      *time.Location
	reset    Reset
	log      *slog.Logger
	readOnly bool

	saveMu sync.Mutex // serializes writers so an older snapshot never lands last

	mu       sync.Mutex
	st       state
	dirty    bool
	lastSave time.Time
}

// Open loads the state file at path, creating an empty state if it doesn't
// exist. A file that can't be parsed is moved aside (never deleted) and
// accounting starts fresh.
func Open(path string, loc *time.Location, reset Reset, log *slog.Logger) (*Accountant, error) {
	return load(path, loc, reset, log, false)
}

// OpenReadOnly loads the state like Open but never modifies the file: a
// corrupt file is left in place and Save is a no-op. Used by dry runs, which
// must not race the real agent's increments or leave a root-owned file behind.
func OpenReadOnly(path string, loc *time.Location, reset Reset, log *slog.Logger) (*Accountant, error) {
	return load(path, loc, reset, log, true)
}

func load(path string, loc *time.Location, reset Reset, log *slog.Logger, readOnly bool) (*Accountant, error) {
	if reset.Day < 1 || reset.Day > 31 {
		return nil, fmt.Errorf("traffic: reset_day %d out of range 1-31", reset.Day)
	}
	if reset.Hour < 0 || reset.Hour > 23 || reset.Minute < 0 || reset.Minute > 59 {
		return nil, fmt.Errorf("traffic: reset time %02d:%02d out of range", reset.Hour, reset.Minute)
	}
	a := &Accountant{
		path:     path,
		loc:      loc,
		reset:    reset,
		log:      log,
		readOnly: readOnly,
		st:       state{Version: stateVersion, Interfaces: map[string]*ifaceState{}},
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Info("traffic state not found, starting fresh", "path", path)
		return a, nil
	case err != nil:
		return nil, fmt.Errorf("traffic: read state: %w", err)
	}

	var st state
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion || st.Interfaces == nil {
		if err == nil {
			err = fmt.Errorf("unexpected version %d", st.Version)
		}
		if readOnly {
			log.Error("traffic state unreadable, starting from empty (read-only, file left in place)", "err", err)
			return a, nil
		}
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("traffic: state file unreadable (%v) and could not be moved aside: %w", err, rerr)
		}
		log.Error("traffic state unreadable, moved aside and starting fresh", "err", err, "moved_to", aside)
		return a, nil
	}
	for name, is := range st.Interfaces {
		if is == nil {
			delete(st.Interfaces, name)
			continue
		}
		if is.Periods == nil {
			is.Periods = map[string]*Totals{}
		}
		for k, t := range is.Periods {
			if t == nil {
				delete(is.Periods, k)
			}
		}
	}
	a.st = st
	return a, nil
}

// Update folds a sample into the running totals.
func (a *Accountant) Update(s Sample) error {
	if s.BootID == "" {
		return errors.New("traffic: empty boot id")
	}
	periodStart := PeriodStart(s.Time, a.loc, a.reset)
	key := PeriodKey(periodStart)

	a.mu.Lock()
	defer a.mu.Unlock()
	for name, cur := range s.Counters {
		is := a.st.Interfaces[name]
		var d Counter
		switch {
		case is == nil:
			is = &ifaceState{Periods: map[string]*Totals{}}
			a.st.Interfaces[name] = is
			if !s.BootTime.IsZero() && !s.BootTime.Before(periodStart) {
				d = cur
			}
			a.log.Info("traffic: new interface", "iface", name, "counted_since_boot", d != Counter{})
		case is.BootID != s.BootID:
			d = cur
			a.log.Info("traffic: reboot detected", "iface", name, "since_boot_rx", cur.RX, "since_boot_tx", cur.TX)
		default:
			d.RX = delta(is.LastRX, cur.RX)
			d.TX = delta(is.LastTX, cur.TX)
		}
		is.BootID = s.BootID
		is.LastRX, is.LastTX = cur.RX, cur.TX

		t := is.Periods[key]
		if t == nil {
			t = &Totals{}
			is.Periods[key] = t
			prune(is.Periods)
		}
		t.RX += d.RX
		t.TX += d.TX
	}
	a.dirty = true
	return nil
}

func delta(last, cur uint64) uint64 {
	if cur >= last {
		return cur - last
	}
	return cur // counter restarted
}

func prune(p map[string]*Totals) {
	if len(p) <= KeepPeriods {
		return
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys) // YYYY-MM-DD sorts chronologically
	for _, k := range keys[:len(keys)-KeepPeriods] {
		delete(p, k)
	}
}

// Snapshot returns current and previous period totals, sorted by interface
// name, for the monitored interfaces plus any other interface that has data
// in either period. The latter keeps a month's traffic visible when an
// interface disappears or is renamed mid-period.
func (a *Accountant) Snapshot(now time.Time, monitored []string) []IfaceTotals {
	curStart := PeriodStart(now, a.loc, a.reset)
	curKey := PeriodKey(curStart)
	prevKey := PeriodKey(PrevPeriodStart(curStart, a.loc, a.reset))

	a.mu.Lock()
	defer a.mu.Unlock()
	names := slices.Clone(monitored)
	for name, is := range a.st.Interfaces {
		if is.Periods[curKey] != nil || is.Periods[prevKey] != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	out := make([]IfaceTotals, 0, len(names))
	for _, name := range names {
		it := IfaceTotals{Iface: name, CurStart: curKey, PrevStart: prevKey}
		if is := a.st.Interfaces[name]; is != nil {
			if t := is.Periods[curKey]; t != nil {
				it.Cur = *t
			}
			if t := is.Periods[prevKey]; t != nil {
				it.Prev = *t
			}
		}
		out = append(out, it)
	}
	return out
}

// MaybeSave persists the state if it changed and SaveEvery has elapsed.
func (a *Accountant) MaybeSave(now time.Time) error {
	a.mu.Lock()
	due := a.dirty && now.Sub(a.lastSave) >= SaveEvery
	a.mu.Unlock()
	if !due {
		return nil
	}
	return a.Save()
}

// Save persists the state atomically: temp file, fsync, rename, fsync dir.
func (a *Accountant) Save() error {
	if a.readOnly {
		return nil
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()

	a.mu.Lock()
	data, err := json.MarshalIndent(a.st, "", "  ")
	if err == nil {
		// Cleared before writing so an Update racing with the write marks the
		// state dirty again rather than being forgotten.
		a.dirty = false
	}
	a.lastSave = time.Now()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.path, data); err != nil {
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
		return fmt.Errorf("traffic: save state: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
