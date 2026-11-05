package watch

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"opsagent/internal/config"
	"opsagent/internal/tools"
)

func watchFixture() (config.Config, tools.Metrics, time.Time) {
	cfg := config.Default("/work")
	cfg.Watch.Enabled = true
	cfg.Watch.QuietStart, cfg.Watch.QuietEnd = "", ""
	cfg.Watch.Consecutive = 2
	cfg.Watch.CooldownSeconds = 60
	m := tools.Metrics{OS: "linux", CPUPercent: 99, MemoryPercent: 20, DiskPercent: 30}
	return cfg, m, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
}

func checkAt(t *testing.T, w *Watcher, now time.Time) {
	t.Helper()
	if err := w.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledNeverSamples(t *testing.T) {
	cfg := config.Default("/work")
	calls := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { calls++; return tools.Metrics{}, nil }, func(Alert) { t.Fatal("disabled watcher emitted") })
	checkAt(t, w, time.Now())
	if calls != 0 {
		t.Fatal("disabled watcher sampled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	if calls != 0 {
		t.Fatal("canceled run sampled")
	}
}

func TestConsecutiveCooldownAndRecovery(t *testing.T) {
	cfg, metrics, now := watchFixture()
	var alerts []Alert
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(a Alert) { alerts = append(alerts, a) })
	checkAt(t, w, now)
	if len(alerts) != 0 {
		t.Fatal("alert before consecutive threshold")
	}
	checkAt(t, w, now.Add(30*time.Second))
	if len(alerts) != 1 || alerts[0].State != "alert" || alerts[0].Metric != "cpu_percent" {
		t.Fatalf("first alert: %+v", alerts)
	}
	checkAt(t, w, now.Add(60*time.Second))
	if len(alerts) != 1 {
		t.Fatal("cooldown ignored")
	}
	checkAt(t, w, now.Add(90*time.Second))
	if len(alerts) != 2 {
		t.Fatal("ongoing issue not repeated after cooldown")
	}
	metrics.CPUPercent = 10
	checkAt(t, w, now.Add(120*time.Second))
	if len(alerts) != 2 {
		t.Fatal("recovery before consecutive samples")
	}
	checkAt(t, w, now.Add(150*time.Second))
	if len(alerts) != 3 || alerts[2].State != "recovered" {
		t.Fatalf("missing recovery: %+v", alerts)
	}
	checkAt(t, w, now.Add(210*time.Second))
	if len(alerts) != 3 {
		t.Fatal("duplicate recovery")
	}
	if alerts[0].ID == alerts[1].ID || alerts[1].ID == alerts[2].ID {
		t.Fatal("duplicate notification ids")
	}
	if s := w.Snapshot(); s.SentToday != 3 || s.Metrics["cpu_percent"].Notified {
		t.Fatalf("wrong state: %+v", s)
	}
}

func TestRecoveryWaitsForCooldown(t *testing.T) {
	cfg, metrics, now := watchFixture()
	cfg.Watch.Consecutive = 1
	var alerts []Alert
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(a Alert) { alerts = append(alerts, a) })
	checkAt(t, w, now)
	metrics.CPUPercent = 1
	checkAt(t, w, now.Add(time.Second))
	if len(alerts) != 1 {
		t.Fatal("recovery bypassed cooldown")
	}
	checkAt(t, w, now.Add(time.Minute))
	if len(alerts) != 2 || alerts[1].State != "recovered" {
		t.Fatal("deferred recovery lost")
	}
}

func TestQuietHoursMidnightAndBoundaries(t *testing.T) {
	cfg, metrics, _ := watchFixture()
	cfg.Watch.Consecutive = 1
	cfg.Watch.QuietStart, cfg.Watch.QuietEnd = "23:00", "08:00"
	loc := time.FixedZone("local", 8*3600)
	now := time.Date(2026, 11, 4, 23, 0, 0, 0, loc)
	var alerts []Alert
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(a Alert) { alerts = append(alerts, a) })
	checkAt(t, w, now)
	checkAt(t, w, now.Add(8*time.Hour+59*time.Minute))
	if len(alerts) != 0 {
		t.Fatal("quiet hours emitted")
	}
	checkAt(t, w, now.Add(9*time.Hour))
	if len(alerts) != 1 || alerts[0].At.Hour() != 8 {
		t.Fatal("quiet-end alert missing")
	}
	metrics.CPUPercent = 1
	checkAt(t, w, now.Add(24*time.Hour))
	if len(alerts) != 1 {
		t.Fatal("quiet recovery emitted")
	}
	checkAt(t, w, now.Add(33*time.Hour))
	if len(alerts) != 2 || alerts[1].State != "recovered" {
		t.Fatal("deferred quiet recovery lost")
	}
}

func TestQuietIncidentDoesNotInventRecovery(t *testing.T) {
	cfg, metrics, _ := watchFixture()
	cfg.Watch.Consecutive = 1
	cfg.Watch.QuietStart, cfg.Watch.QuietEnd = "23:00", "08:00"
	now := time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC)
	count := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(Alert) { count++ })
	checkAt(t, w, now)
	metrics.CPUPercent = 1
	checkAt(t, w, now.Add(time.Hour))
	checkAt(t, w, now.Add(9*time.Hour))
	if count != 0 {
		t.Fatal("recovered notification for never-announced incident")
	}
}

func TestDailyCapIncludesRecoveryAndResets(t *testing.T) {
	cfg, metrics, now := watchFixture()
	cfg.Watch.Consecutive = 1
	cfg.Watch.DailyLimit = 1
	var alerts []Alert
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(a Alert) { alerts = append(alerts, a) })
	checkAt(t, w, now)
	metrics.CPUPercent = 10
	checkAt(t, w, now.Add(time.Minute))
	if len(alerts) != 1 {
		t.Fatal("recovery bypassed daily limit")
	}
	checkAt(t, w, now.AddDate(0, 0, 1))
	if len(alerts) != 2 || alerts[1].State != "recovered" {
		t.Fatal("new-day deferred recovery missing")
	}
	if w.Snapshot().SentToday != 1 {
		t.Fatal("day count not reset")
	}
}

func TestFailedAndUnsupportedSamplesBreakStreak(t *testing.T) {
	cfg, metrics, now := watchFixture()
	var sampleErr error
	count := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, sampleErr }, func(Alert) { count++ })
	checkAt(t, w, now)
	sampleErr = errors.New("metrics unavailable")
	if err := w.Check(context.Background(), now.Add(time.Second)); err == nil {
		t.Fatal("sample error hidden")
	}
	sampleErr = nil
	checkAt(t, w, now.Add(2*time.Second))
	if count != 0 {
		t.Fatal("failed sample counted toward streak")
	}
	metrics.Unsupported = []string{"cpu_percent"}
	checkAt(t, w, now.Add(3*time.Second))
	metrics.Unsupported = nil
	checkAt(t, w, now.Add(4*time.Second))
	if count != 0 {
		t.Fatal("unsupported sample counted toward streak")
	}
	checkAt(t, w, now.Add(5*time.Second))
	if count != 1 {
		t.Fatal("valid consecutive samples failed to alert")
	}
	metrics.CPUPercent = 0
	metrics.Unsupported = []string{"cpu_percent"}
	checkAt(t, w, now.Add(time.Minute))
	checkAt(t, w, now.Add(2*time.Minute))
	if count != 1 {
		t.Fatal("unsupported zero created recovery")
	}
}

func TestNonfiniteMetricsDoNotAlert(t *testing.T) {
	cfg, metrics, now := watchFixture()
	cfg.Watch.Consecutive = 1
	count := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(Alert) { count++ })
	for i, value := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		metrics.CPUPercent = value
		checkAt(t, w, now.Add(time.Duration(i)*time.Minute))
	}
	if count != 0 {
		t.Fatal("invalid metric emitted")
	}
}

func TestReconfigureResetsStreakWithoutResettingBudget(t *testing.T) {
	cfg, metrics, now := watchFixture()
	var alerts []Alert
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(a Alert) { alerts = append(alerts, a) })
	checkAt(t, w, now)
	cfg.Watch.CPUPercent = 90
	checkAt(t, w, now.Add(time.Minute))
	if len(alerts) != 0 {
		t.Fatal("old threshold streak survived reconfiguration")
	}
	checkAt(t, w, now.Add(2*time.Minute))
	if len(alerts) != 1 {
		t.Fatal("new threshold did not alert")
	}
	cfg.Watch.Enabled = false
	checkAt(t, w, now.Add(3*time.Minute))
	cfg.Watch.Enabled = true
	metrics.CPUPercent = 1
	checkAt(t, w, now.Add(4*time.Minute))
	checkAt(t, w, now.Add(5*time.Minute))
	if len(alerts) != 2 || alerts[1].State != "recovered" || w.Snapshot().SentToday != 2 {
		t.Fatal("reconfiguration lost notified episode/budget")
	}
}

func TestReconfigureDuringSampleDiscardsData(t *testing.T) {
	cfg, metrics, now := watchFixture()
	cfg.Watch.Consecutive = 1
	count := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { cfg.Watch.Enabled = false; return metrics, nil }, func(Alert) { count++ })
	checkAt(t, w, now)
	if count != 0 {
		t.Fatal("sample emitted after disable")
	}
}

func TestSnapshotRestoreAndIsolation(t *testing.T) {
	cfg, metrics, now := watchFixture()
	cfg.Watch.Consecutive = 1
	count := 0
	var w *Watcher
	w = New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(Alert) { count++; _ = w.Snapshot() })
	checkAt(t, w, now)
	state := w.Snapshot()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	other := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, func(Alert) { count++ })
	other.Restore(restored)
	restored.Metrics["cpu_percent"] = MetricState{}
	state.Metrics["cpu_percent"] = MetricState{}
	checkAt(t, other, now.Add(time.Second))
	if count != 1 {
		t.Fatal("restart produced cooldown storm")
	}
	checkAt(t, other, now.Add(time.Minute))
	if count != 2 || other.Snapshot().SentToday != 2 {
		t.Fatal("restored cooldown/budget incorrect")
	}
	if !w.Snapshot().Metrics["cpu_percent"].Notified {
		t.Fatal("snapshot aliases live state")
	}
}

func TestDuplicateAndBackwardTimeIgnored(t *testing.T) {
	cfg, metrics, now := watchFixture()
	calls := 0
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { calls++; return metrics, nil }, nil)
	checkAt(t, w, now)
	checkAt(t, w, now)
	checkAt(t, w, now.Add(-time.Hour))
	if calls != 1 {
		t.Fatal("duplicate/backward clock sampled")
	}
}

func TestQuietValidationAndEqualRange(t *testing.T) {
	cfg, _, now := watchFixture()
	for _, value := range []string{"3:00", "23:0x", "24:00", "12:60", "ab:cd"} {
		cfg.Watch.QuietStart, cfg.Watch.QuietEnd = value, "08:00"
		if _, err := normalize(cfg.Watch); err == nil {
			t.Errorf("accepted invalid quiet time %q", value)
		}
	}
	cfg.Watch.QuietStart, cfg.Watch.QuietEnd = "12:00", "12:00"
	if inQuiet(now, cfg.Watch) {
		t.Fatal("equal range should disable quiet hours")
	}
}

func TestConcurrentChecksAndSnapshots(t *testing.T) {
	cfg, metrics, now := watchFixture()
	w := New(func() config.Config { return cfg }, func(context.Context) (tools.Metrics, error) { return metrics, nil }, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.Check(context.Background(), now.Add(time.Duration(i)*time.Second))
			_ = w.Snapshot()
		}(i)
	}
	wg.Wait()
}
