// Package watch implements read-only host-metric monitoring. It never calls a
// model, starts a command, or writes a file. Persistence and delivery are the
// caller's responsibility through Snapshot/Restore and emit.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"opsagent/internal/config"
	"opsagent/internal/tools"
)

type Alert struct {
	ID        string    `json:"id"`
	Metric    string    `json:"metric"`
	State     string    `json:"state"` // "alert" or "recovered"
	Message   string    `json:"message"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	At        time.Time `json:"at"`
}

type MetricState struct {
	HighStreak int       `json:"high_streak"`
	LowStreak  int       `json:"low_streak"`
	Active     bool      `json:"active"`
	Notified   bool      `json:"notified"`
	LastSent   time.Time `json:"last_sent"`
	LastValue  float64   `json:"last_value"`
}

// State is JSON-friendly and owns no live handles. Persist after each Check (or
// emission) and Restore before Run to retain cooldowns, episodes and daily caps.
// Without Restore state is intentionally process-local. Delivery is at-most-once
// per in-memory transition; durable exactly-once delivery needs a caller outbox.
type State struct {
	Version   int                    `json:"version"`
	Day       string                 `json:"day"`
	SentToday int                    `json:"sent_today"`
	Sequence  uint64                 `json:"sequence"`
	LastCheck time.Time              `json:"last_check"`
	RulesKey  string                 `json:"rules_key"`
	Metrics   map[string]MetricState `json:"metrics"`
}

type Watcher struct {
	getConfig func() config.Config
	sample    func(context.Context) (tools.Metrics, error)
	emit      func(Alert)
	checkMu   sync.Mutex
	mu        sync.Mutex
	state     State
}

var metricNames = []string{"cpu_percent", "memory_percent", "disk_percent"}

func New(getConfig func() config.Config, sample func(context.Context) (tools.Metrics, error), emit func(Alert)) *Watcher {
	return &Watcher{getConfig: getConfig, sample: sample, emit: emit, state: State{Version: 1, Metrics: make(map[string]MetricState)}}
}

func (w *Watcher) Snapshot() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneState(w.state)
}

// Restore ignores incompatible or structurally invalid snapshots. Never pass an
// untrusted snapshot: it is operational state, not a source of configuration.
func (w *Watcher) Restore(state State) {
	if state.Version != 1 || state.SentToday < 0 || state.SentToday > 100 {
		return
	}
	if state.Day != "" {
		if _, err := time.Parse("2006-01-02", state.Day); err != nil {
			return
		}
	}
	clean := cloneState(state)
	clean.Metrics = make(map[string]MetricState)
	for _, name := range metricNames {
		s, ok := state.Metrics[name]
		if !ok {
			continue
		}
		if s.HighStreak < 0 || s.HighStreak > 20 || s.LowStreak < 0 || s.LowStreak > 20 || math.IsNaN(s.LastValue) || math.IsInf(s.LastValue, 0) {
			return
		}
		clean.Metrics[name] = s
	}
	w.checkMu.Lock()
	defer w.checkMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = clean
}

func cloneState(s State) State {
	out := s
	out.Metrics = make(map[string]MetricState, len(s.Metrics))
	for key, value := range s.Metrics {
		out.Metrics[key] = value
	}
	return out
}

func (w *Watcher) settings() config.Watch {
	if w.getConfig == nil {
		return config.Watch{}
	}
	return w.getConfig().Watch
}

func settingsKey(c config.Watch) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func normalize(c config.Watch) (config.Watch, error) {
	if c.IntervalSeconds <= 0 {
		c.IntervalSeconds = 300
	}
	c.IntervalSeconds = max(30, min(c.IntervalSeconds, 86400))
	if c.Consecutive <= 0 {
		c.Consecutive = 2
	}
	c.Consecutive = min(c.Consecutive, 20)
	if c.CooldownSeconds <= 0 {
		c.CooldownSeconds = 1800
	}
	c.CooldownSeconds = max(60, min(c.CooldownSeconds, 365*24*60*60))
	if c.DailyLimit <= 0 {
		c.DailyLimit = 10
	}
	c.DailyLimit = min(c.DailyLimit, 100)
	for _, n := range []float64{c.CPUPercent, c.MemoryPercent, c.DiskPercent} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 100 {
			return c, errors.New("invalid watch threshold")
		}
	}
	if (c.QuietStart == "") != (c.QuietEnd == "") {
		return c, errors.New("quiet hours need both start and end")
	}
	if c.QuietStart != "" {
		if _, err := parseClock(c.QuietStart); err != nil {
			return c, err
		}
		if _, err := parseClock(c.QuietEnd); err != nil {
			return c, err
		}
	}
	return c, nil
}

func parseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' || s[3] < '0' || s[3] > '9' || s[4] < '0' || s[4] > '9' {
		return 0, errors.New("invalid quiet hours")
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, errors.New("invalid quiet hours")
	}
	return h*60 + m, nil
}

// Quiet times use now's location. Equal start/end explicitly disables quiet hours.
// Start is inclusive, end is exclusive; ranges spanning midnight are supported.
func inQuiet(now time.Time, c config.Watch) bool {
	if c.QuietStart == "" {
		return false
	}
	start, _ := parseClock(c.QuietStart)
	end, _ := parseClock(c.QuietEnd)
	minute := now.Hour()*60 + now.Minute()
	if start == end {
		return false
	}
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

func (w *Watcher) applySettings(key string) {
	if w.state.RulesKey == key {
		return
	}
	// Reconfiguration resets streaks, not notification budgets/cooldowns or the
	// fact that an episode was announced. Old thresholds cannot create streaks
	// under new rules; an announced episode can still recover under new rules.
	for name, s := range w.state.Metrics {
		s.HighStreak, s.LowStreak = 0, 0
		w.state.Metrics[name] = s
	}
	w.state.RulesKey = key
}

func unsupported(m tools.Metrics, name string) bool {
	for _, item := range m.Unsupported {
		item = strings.ToLower(item)
		if item == name || item == strings.TrimSuffix(name, "_percent") || strings.HasPrefix(item, name+":") {
			return true
		}
	}
	return false
}

// Check samples at the caller-supplied time, making the state machine testable
// without sleeps. Both alerts and recoveries require consecutive valid samples;
// both obey quiet hours, per-metric cooldowns and a shared daily notification cap.
// Unsupported/failed samples break a streak and never synthesize a recovery.
// No sampling occurs while disabled. Duplicate/backward times do not resample.
func (w *Watcher) Check(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.checkMu.Lock()
	defer w.checkMu.Unlock()
	cfg := w.settings()
	key := settingsKey(cfg)
	w.mu.Lock()
	w.applySettings(key)
	if !cfg.Enabled {
		w.mu.Unlock()
		return nil
	}
	if !w.state.LastCheck.IsZero() && !now.After(w.state.LastCheck) {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()
	cfg, err := normalize(cfg)
	if err != nil {
		return err
	}
	if w.sample == nil {
		return errors.New("watch sampler is not configured")
	}
	metrics, err := w.sample(ctx)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	// A configuration change during a sample invalidates that sample.
	latest := w.settings()
	w.mu.Lock()
	if settingsKey(latest) != key || !latest.Enabled {
		w.applySettings(settingsKey(latest))
		w.mu.Unlock()
		return err
	}
	w.state.LastCheck = now
	if err != nil {
		for name, s := range w.state.Metrics {
			s.HighStreak, s.LowStreak = 0, 0
			w.state.Metrics[name] = s
		}
		w.mu.Unlock()
		return err
	}
	day := now.Format("2006-01-02")
	if w.state.Day == "" || day > w.state.Day {
		w.state.Day, w.state.SentToday = day, 0
	}
	quiet := inQuiet(now, cfg)
	var alerts []Alert
	values := []float64{metrics.CPUPercent, metrics.MemoryPercent, metrics.DiskPercent}
	thresholds := []float64{cfg.CPUPercent, cfg.MemoryPercent, cfg.DiskPercent}
	for i, name := range metricNames {
		s := w.state.Metrics[name]
		value, threshold := values[i], thresholds[i]
		if unsupported(metrics, name) || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			s.HighStreak, s.LowStreak = 0, 0
			w.state.Metrics[name] = s
			continue
		}
		s.LastValue = value
		if value >= threshold {
			s.HighStreak, s.LowStreak = min(s.HighStreak+1, cfg.Consecutive), 0
			if s.HighStreak >= cfg.Consecutive {
				s.Active = true
			}
		} else {
			s.LowStreak, s.HighStreak = min(s.LowStreak+1, cfg.Consecutive), 0
			if s.LowStreak >= cfg.Consecutive {
				s.Active = false
			}
		}
		state := ""
		if s.Active && value >= threshold && s.HighStreak >= cfg.Consecutive {
			state = "alert"
		}
		if !s.Active && s.Notified && s.LowStreak >= cfg.Consecutive {
			state = "recovered"
		}
		cooled := s.LastSent.IsZero() || (!now.Before(s.LastSent) && now.Sub(s.LastSent) >= time.Duration(cfg.CooldownSeconds)*time.Second)
		if state != "" && !quiet && cooled && w.state.SentToday < cfg.DailyLimit && w.emit != nil {
			w.state.Sequence++
			alert := Alert{ID: fmt.Sprintf("watch-%d-%d-%s", now.UnixNano(), w.state.Sequence, name), Metric: name, State: state, Value: value, Threshold: threshold, At: now}
			if state == "alert" {
				alert.Message = fmt.Sprintf("%s %.1f%% reached threshold %.1f%% for %d consecutive samples", name, value, threshold, cfg.Consecutive)
			} else {
				alert.Message = fmt.Sprintf("%s recovered to %.1f%% (threshold %.1f%%)", name, value, threshold)
			}
			s.LastSent, s.Notified = now, state == "alert"
			w.state.SentToday++
			alerts = append(alerts, alert)
		}
		w.state.Metrics[name] = s
	}
	w.mu.Unlock()
	// State is committed before callbacks; emit may safely call Snapshot.
	for _, alert := range alerts {
		w.emit(alert)
	}
	return nil
}

// Run uses a short configuration poll, not a sampling poll. Disabled watchers
// collect no data. Config changes wake a sample promptly; normal sampling obeys
// the configured interval. The caller owns logging/handling Check errors.
func (w *Watcher) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var next time.Time
	previous := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		cfg := w.settings()
		key := settingsKey(cfg)
		now := time.Now()
		if key != previous {
			next = time.Time{}
			previous = key
		}
		if !cfg.Enabled {
			_ = w.Check(ctx, now)
			next = time.Time{}
		} else if next.IsZero() || !now.Before(next) {
			_ = w.Check(ctx, now)
			seconds := cfg.IntervalSeconds
			if seconds <= 0 {
				seconds = 300
			}
			next = now.Add(time.Duration(max(30, min(seconds, 86400))) * time.Second)
		}
		timer.Reset(time.Second)
	}
}
