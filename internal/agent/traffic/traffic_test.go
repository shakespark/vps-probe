package traffic

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	sh, _   = time.LoadLocation("Asia/Shanghai")
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, sh)
	if err != nil {
		panic(err)
	}
	return t
}

func open(t *testing.T, path string, resetDay int) *Accountant {
	t.Helper()
	return openReset(t, path, Reset{Day: resetDay})
}

func openReset(t *testing.T, path string, r Reset) *Accountant {
	t.Helper()
	a, err := Open(path, sh, r, discard)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sample(ts, boot string, bootTime string, rx, tx uint64) Sample {
	return Sample{
		Time:     at(ts),
		BootID:   boot,
		BootTime: at(bootTime),
		Counters: map[string]Counter{"eth0": {RX: rx, TX: tx}},
	}
}

func cur(t *testing.T, a *Accountant, now string) Totals {
	t.Helper()
	return a.Snapshot(at(now), []string{"eth0"})[0].Cur
}

func mustUpdate(t *testing.T, a *Accountant, s Sample) {
	t.Helper()
	if err := a.Update(s); err != nil {
		t.Fatal(err)
	}
}

func TestSameBootAccumulates(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	// Booted this month: bytes since boot count.
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-09 00:00:00", 1000, 500))
	mustUpdate(t, a, sample("2026-09-10 00:00:10", "b1", "2026-09-09 00:00:00", 1500, 700))
	mustUpdate(t, a, sample("2026-09-10 00:00:20", "b1", "2026-09-09 00:00:00", 4000, 800))
	if got := cur(t, a, "2026-09-10 00:00:20"); got != (Totals{RX: 4000, TX: 800}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFirstSeenBootedBeforePeriodTakesBaseline(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	// Booted last month: unknown split, so only a baseline.
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-08-20 00:00:00", 1_000_000, 900_000))
	mustUpdate(t, a, sample("2026-09-10 00:00:10", "b1", "2026-08-20 00:00:00", 1_000_100, 900_050))
	if got := cur(t, a, "2026-09-10 00:00:10"); got != (Totals{RX: 100, TX: 50}) {
		t.Fatalf("got %+v", got)
	}
}

func TestRebootAddsSinceBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10_000, 5_000))
	mustUpdate(t, a, sample("2026-09-10 01:00:00", "b1", "2026-09-01 00:00:00", 30_000, 8_000))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	// Machine reboots; agent restarts and loads state from disk.
	b := open(t, path, 1)
	mustUpdate(t, b, sample("2026-09-10 01:05:00", "b2", "2026-09-10 01:04:00", 200, 100))
	mustUpdate(t, b, sample("2026-09-10 01:05:10", "b2", "2026-09-10 01:04:00", 700, 300))
	if got := cur(t, b, "2026-09-10 01:05:10"); got != (Totals{RX: 30_700, TX: 8_300}) {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentRestartSameBootCatchesUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10_000, 5_000))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	// Agent was down for an hour, same boot: the gap is recovered from the
	// kernel counter.
	b := open(t, path, 1)
	mustUpdate(t, b, sample("2026-09-10 01:00:00", "b1", "2026-09-01 00:00:00", 90_000, 45_000))
	if got := cur(t, b, "2026-09-10 01:00:00"); got != (Totals{RX: 90_000, TX: 45_000}) {
		t.Fatalf("got %+v", got)
	}
}

func TestCounterResetWithinBoot(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10_000, 5_000))
	// Driver reload: counters restart from zero.
	mustUpdate(t, a, sample("2026-09-10 00:00:10", "b1", "2026-09-01 00:00:00", 300, 200))
	mustUpdate(t, a, sample("2026-09-10 00:00:20", "b1", "2026-09-01 00:00:00", 400, 250))
	if got := cur(t, a, "2026-09-10 00:00:20"); got != (Totals{RX: 10_400, TX: 5_250}) {
		t.Fatalf("got %+v", got)
	}
}

func TestRXAndTXResetIndependently(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10_000, 5_000))
	mustUpdate(t, a, sample("2026-09-10 00:00:10", "b1", "2026-09-01 00:00:00", 10_500, 100))
	if got := cur(t, a, "2026-09-10 00:00:10"); got != (Totals{RX: 10_500, TX: 5_100}) {
		t.Fatalf("got %+v", got)
	}
}

func TestMonthRollover(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	mustUpdate(t, a, sample("2026-08-31 23:59:50", "b1", "2026-08-01 00:00:00", 1000, 100))
	mustUpdate(t, a, sample("2026-09-01 00:00:00", "b1", "2026-08-01 00:00:00", 1500, 150))
	mustUpdate(t, a, sample("2026-09-01 00:00:10", "b1", "2026-08-01 00:00:00", 1700, 160))

	snap := a.Snapshot(at("2026-09-01 00:00:10"), []string{"eth0"})[0]
	if snap.CurStart != "2026-09-01" || snap.PrevStart != "2026-08-01" {
		t.Fatalf("keys: %+v", snap)
	}
	if snap.Prev != (Totals{RX: 1000, TX: 100}) {
		t.Fatalf("prev = %+v", snap.Prev)
	}
	if snap.Cur != (Totals{RX: 700, TX: 60}) {
		t.Fatalf("cur = %+v", snap.Cur)
	}
}

func TestResetDay(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 15)
	mustUpdate(t, a, sample("2026-09-14 23:59:50", "b1", "2026-09-01 00:00:00", 1000, 0))
	mustUpdate(t, a, sample("2026-09-15 00:00:00", "b1", "2026-09-01 00:00:00", 1200, 0))
	snap := a.Snapshot(at("2026-09-15 00:00:00"), []string{"eth0"})[0]
	if snap.CurStart != "2026-09-15" || snap.Cur.RX != 200 {
		t.Fatalf("cur: %+v", snap)
	}
	if snap.PrevStart != "2026-08-15" || snap.Prev.RX != 1000 {
		t.Fatalf("prev: %+v", snap)
	}
}

func TestResetTime(t *testing.T) {
	a := openReset(t, filepath.Join(t.TempDir(), "s.json"), Reset{Day: 21, Hour: 18, Minute: 21})
	mustUpdate(t, a, sample("2026-10-21 18:20:50", "b1", "2026-10-01 00:00:00", 1000, 0))
	mustUpdate(t, a, sample("2026-10-21 18:21:00", "b1", "2026-10-01 00:00:00", 1200, 0))
	snap := a.Snapshot(at("2026-10-21 18:21:00"), []string{"eth0"})[0]
	if snap.CurStart != "2026-10-21" || snap.Cur.RX != 200 {
		t.Fatalf("cur: %+v", snap)
	}
	if snap.PrevStart != "2026-09-21" || snap.Prev.RX != 1000 {
		t.Fatalf("prev: %+v", snap)
	}
}

func TestMultipleInterfacesAndNewInterface(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	boot := at("2026-09-01 08:00:00")
	mustUpdate(t, a, Sample{Time: at("2026-09-10 00:00:00"), BootID: "b1", BootTime: boot,
		Counters: map[string]Counter{"eth0": {100, 10}}})
	// eth1 appears later in the same boot; the machine booted in this period,
	// so its since-boot bytes count.
	mustUpdate(t, a, Sample{Time: at("2026-09-10 00:00:10"), BootID: "b1", BootTime: boot,
		Counters: map[string]Counter{"eth0": {150, 20}, "eth1": {5000, 4000}}})
	snap := a.Snapshot(at("2026-09-10 00:00:10"), []string{"eth9", "eth0", "eth1"})
	want := map[string]Totals{"eth0": {150, 20}, "eth1": {5000, 4000}, "eth9": {}}
	if len(snap) != len(want) {
		t.Fatalf("got %d interfaces", len(snap))
	}
	for i, s := range snap {
		if i > 0 && snap[i-1].Iface >= s.Iface {
			t.Errorf("not sorted: %v", snap)
		}
		if s.Cur != want[s.Iface] {
			t.Errorf("%s = %+v, want %+v", s.Iface, s.Cur, want[s.Iface])
		}
	}
}

func TestSnapshotKeepsRenamedInterface(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	boot := at("2026-09-01 08:00:00")
	mustUpdate(t, a, Sample{Time: at("2026-09-10 00:00:00"), BootID: "b1", BootTime: boot,
		Counters: map[string]Counter{"eth0": {1000, 100}}})
	// After a reboot the NIC comes up as ens3; eth0's month must still be
	// reported.
	mustUpdate(t, a, Sample{Time: at("2026-09-12 00:00:00"), BootID: "b2", BootTime: at("2026-09-11 23:59:00"),
		Counters: map[string]Counter{"ens3": {50, 5}}})
	snap := a.Snapshot(at("2026-09-12 00:00:00"), []string{"ens3"})
	if len(snap) != 2 || snap[0].Iface != "ens3" || snap[1].Iface != "eth0" {
		t.Fatalf("got %+v", snap)
	}
	if snap[1].Cur != (Totals{1000, 100}) {
		t.Fatalf("eth0 = %+v", snap[1].Cur)
	}
	// Two periods later eth0 no longer has recent data and drops out.
	if snap := a.Snapshot(at("2026-11-12 00:00:00"), []string{"ens3"}); len(snap) != 1 {
		t.Fatalf("stale interface still reported: %+v", snap)
	}
}

func TestOpenToleratesNilEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	content := `{"version":1,"interfaces":{"eth0":null,"eth1":{"boot_id":"b1","last_rx":1,"last_tx":1,"periods":{"2026-09-01":null,"2026-08-01":{"rx":5,"tx":6}}}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a := open(t, path, 1)
	snap := a.Snapshot(at("2026-09-10 00:00:00"), nil)
	if len(snap) != 1 || snap[0].Iface != "eth1" || snap[0].Prev != (Totals{5, 6}) {
		t.Fatalf("got %+v", snap)
	}
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10, 10))
}

func TestInterfaceMissingThenBackAfterReboot(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	boot1 := at("2026-09-01 08:00:00")
	mustUpdate(t, a, Sample{Time: at("2026-09-10 00:00:00"), BootID: "b1", BootTime: boot1,
		Counters: map[string]Counter{"eth0": {1000, 0}, "eth1": {5000, 0}}})
	// Reboot; eth1 is absent for a while (e.g. slow to come up).
	boot2 := at("2026-09-11 00:00:00")
	mustUpdate(t, a, Sample{Time: at("2026-09-11 00:01:00"), BootID: "b2", BootTime: boot2,
		Counters: map[string]Counter{"eth0": {10, 0}}})
	// eth1 returns with a small counter from the new boot. It must be treated
	// as a reboot for eth1 (delta = cur), not as cur-last in the same boot.
	mustUpdate(t, a, Sample{Time: at("2026-09-11 00:02:00"), BootID: "b2", BootTime: boot2,
		Counters: map[string]Counter{"eth0": {20, 0}, "eth1": {300, 0}}})
	snap := a.Snapshot(at("2026-09-11 00:02:00"), []string{"eth0", "eth1"})
	if snap[0].Cur.RX != 1020 || snap[1].Cur.RX != 5300 {
		t.Fatalf("got %+v", snap)
	}
}

func TestEmptyBootIDRejected(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	if err := a.Update(Sample{Time: at("2026-09-10 00:00:00")}); err == nil {
		t.Fatal("accepted empty boot id")
	}
}

func TestPrunesOldPeriods(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "s.json"), 1)
	start := at("2024-01-10 00:00:00")
	var rx uint64
	for i := 0; i < KeepPeriods+6; i++ {
		rx += 100
		mustUpdate(t, a, Sample{Time: start.AddDate(0, i, 0), BootID: "b1", BootTime: start,
			Counters: map[string]Counter{"eth0": {rx, 0}}})
	}
	p := a.st.Interfaces["eth0"].Periods
	if len(p) != KeepPeriods {
		t.Fatalf("kept %d periods", len(p))
	}
	if _, ok := p["2024-01-01"]; ok {
		t.Fatal("oldest period not pruned")
	}
	if _, ok := p["2026-05-01"]; !ok {
		t.Fatal("newest period missing")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 1234, 5678))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
	b := open(t, path, 1)
	if got := cur(t, b, "2026-09-10 00:00:00"); got != (Totals{1234, 5678}) {
		t.Fatalf("got %+v", got)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries", len(entries))
	}
}

func TestCorruptStateMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	for name, content := range map[string]string{
		"garbage":       "{not json",
		"wrong version": `{"version": 99, "interfaces": {}}`,
		"truncated":     `{"version": 1, "interfaces": {"eth0": {"last_rx": 1`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			a := open(t, path, 1)
			if len(a.st.Interfaces) != 0 {
				t.Fatal("state not fresh")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("corrupt file still in place")
			}
			entries, _ := os.ReadDir(dir)
			found := false
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "s.json.corrupt-") {
					found = true
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
			if !found {
				t.Fatal("corrupt copy not kept")
			}
		})
	}
}

func TestStateFileFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10, 20))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["version"] != float64(1) {
		t.Fatalf("version = %v", raw["version"])
	}
}

func TestMaybeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	now := time.Now()
	if err := a.MaybeSave(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("saved clean state")
	}
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 10, 20))
	if err := a.MaybeSave(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dirty state not saved")
	}
	os.Remove(path)
	mustUpdate(t, a, sample("2026-09-10 00:00:10", "b1", "2026-09-01 00:00:00", 20, 30))
	if err := a.MaybeSave(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("saved before SaveEvery elapsed")
	}
	if err := a.MaybeSave(time.Now().Add(SaveEvery + time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("not saved after SaveEvery")
	}
}

func TestConcurrentUpdateAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := open(t, path, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		base := at("2026-09-10 00:00:00")
		for i := 1; i <= 500; i++ {
			a.Update(Sample{Time: base.Add(time.Duration(i) * time.Second), BootID: "b1",
				BootTime: at("2026-09-01 00:00:00"), Counters: map[string]Counter{"eth0": {uint64(i), 0}}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := a.Save(); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b := open(t, path, 1)
	if got := cur(t, b, "2026-09-10 00:10:00"); got.RX != 500 {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenRejectsBadResetDay(t *testing.T) {
	for _, d := range []int{0, 32, -1} {
		if _, err := Open(filepath.Join(t.TempDir(), "s.json"), sh, Reset{Day: d}, discard); err == nil {
			t.Errorf("reset_day %d accepted", d)
		}
	}
	for _, r := range []Reset{{Day: 1, Hour: 24}, {Day: 1, Minute: 60}, {Day: 1, Hour: -1}} {
		if _, err := Open(filepath.Join(t.TempDir(), "s.json"), sh, r, discard); err == nil {
			t.Errorf("reset %+v accepted", r)
		}
	}
}

func TestReadOnlyNeverWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	a := open(t, path, 1)
	mustUpdate(t, a, sample("2026-09-10 00:00:00", "b1", "2026-09-01 00:00:00", 100, 10))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	ro, err := OpenReadOnly(path, sh, Reset{Day: 1}, discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := cur(t, ro, "2026-09-10 00:00:00"); got != (Totals{100, 10}) {
		t.Fatalf("read-only did not load state: %+v", got)
	}
	mustUpdate(t, ro, sample("2026-09-10 00:00:10", "b1", "2026-09-01 00:00:00", 500, 50))
	if err := ro.Save(); err != nil {
		t.Fatal(err)
	}
	if err := ro.MaybeSave(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("read-only accountant modified the state file")
	}

	// A corrupt file is left in place.
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := OpenReadOnly(path, sh, Reset{Day: 1}, discard); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{broken" {
		t.Fatal("read-only open touched a corrupt file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("read-only open created files: %d entries", len(entries))
	}
}
