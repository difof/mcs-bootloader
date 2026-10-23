package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

func timingValues(points []point) map[string]float64 {
	values := make(map[string]float64)
	for _, p := range points {
		values[p.Name] = p.Value
	}
	return values
}

func TestStartupMetricsUnknownCountdownAndOverdue(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := control.Snapshot{State: control.Starting, BootStartedAt: now.Add(-75 * time.Second)}
	values := timingValues(lifecycleTimingPoints(s, now))
	if values["mcs_startup_estimate_known"] != 0 || values["mcs_startup_elapsed_seconds"] != 75 {
		t.Fatalf("incorrect first-boot state: %+v", values)
	}
	for _, name := range []string{"mcs_startup_estimate_seconds", "mcs_startup_duration_seconds", "mcs_startup_remaining_seconds", "mcs_startup_estimate_overdue"} {
		if _, ok := values[name]; ok {
			t.Fatalf("unknown estimate invented %s", name)
		}
	}
	s.SuccessfulBoots = 2
	s.BootEstimateSeconds = 300
	s.LastBootDurationSeconds = 280
	values = timingValues(lifecycleTimingPoints(s, now))
	for name, want := range map[string]float64{
		"mcs_startup_estimate_known": 1, "mcs_startup_successes_total": 2,
		"mcs_startup_estimate_seconds": 300, "mcs_startup_duration_seconds": 280,
		"mcs_startup_elapsed_seconds": 75, "mcs_startup_remaining_seconds": 225,
		"mcs_startup_estimate_overdue": 0,
	} {
		if got, ok := values[name]; !ok || got != want {
			t.Errorf("%s got %v, present %v, want %v", name, got, ok, want)
		}
	}
	values = timingValues(lifecycleTimingPoints(s, now.Add(4*time.Minute)))
	if values["mcs_startup_remaining_seconds"] != 0 || values["mcs_startup_estimate_overdue"] != 1 || values["mcs_startup_elapsed_seconds"] != 315 {
		t.Fatalf("overdue boot is misleading: %+v", values)
	}
	// An adopted process has no observed current attempt. Its retained history
	// remains useful for the next wake, but must not manufacture a countdown.
	s.BootStartedAt = time.Time{}
	values = timingValues(lifecycleTimingPoints(s, now))
	if _, ok := values["mcs_startup_remaining_seconds"]; ok {
		t.Fatal("adopted process received a guessed countdown")
	}
}

func TestObservedReadyDurationIsDistinctFromBootAndProcessUptime(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true,
		BootStartedAt: now.Add(-10 * time.Minute), ReadySince: now.Add(-5 * time.Minute),
		SuccessfulBoots: 1, BootEstimateSeconds: 300, LastBootDurationSeconds: 300}
	values := timingValues(lifecycleTimingPoints(s, now))
	if values["mcs_controller_ready_duration_seconds"] != 300 {
		t.Fatalf("wrong observed-ready duration: %+v", values)
	}
	for _, name := range []string{"mcs_startup_remaining_seconds", "mcs_startup_elapsed_seconds", "mcs_minecraft_uptime_seconds"} {
		if _, ok := values[name]; ok {
			t.Fatalf("ready controller timing invented %s", name)
		}
	}
	for _, state := range []control.State{control.Unknown, control.Starting, control.Stopping, control.Asleep, control.Failed} {
		s.State = state
		if _, ok := timingValues(lifecycleTimingPoints(s, now))["mcs_controller_ready_duration_seconds"]; ok {
			t.Errorf("ready duration exported for state %s", state)
		}
	}
	for _, estimate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		s.State, s.BootEstimateSeconds = control.Starting, estimate
		values = timingValues(lifecycleTimingPoints(s, now))
		if values["mcs_startup_estimate_known"] != 0 {
			t.Errorf("invalid estimate reported known: %v", estimate)
		}
		if _, ok := values["mcs_startup_remaining_seconds"]; ok {
			t.Errorf("invalid estimate manufactured a countdown: %v", estimate)
		}
	}
}

func TestProcessUptimeUsesCachedJavaStartAndAdvancesWithoutPolling(t *testing.T) {
	now := time.Unix(1700000000, 0)
	start := unix(now.Add(-2 * time.Hour))
	cached := []point{gauge("mcs_jvm_process_start_time_seconds", "test", start)}
	first := timingValues(processUptimePoints("jvm_process", cached, now))
	second := timingValues(processUptimePoints("jvm_process", cached, now.Add(time.Minute)))
	if first["mcs_minecraft_uptime_seconds"] != 7200 || second["mcs_minecraft_uptime_seconds"] != 7260 || second["mcs_minecraft_start_time_seconds"] != start {
		t.Fatalf("uptime did not use stable cached start: first=%+v second=%+v", first, second)
	}
	for _, value := range []float64{0, -1, unix(now.Add(time.Second)), math.NaN(), math.Inf(1)} {
		if got := processUptimePoints("jvm_process", []point{gauge("mcs_jvm_process_start_time_seconds", "test", value)}, now); len(got) != 0 {
			t.Errorf("invalid timestamp produced uptime: %v => %+v", value, got)
		}
	}
	self := timingValues(processUptimePoints("self", []point{gauge("process_start_time_seconds", "test", unix(now.Add(-time.Minute)))}, now))
	if self["process_uptime_seconds"] != 60 {
		t.Fatalf("missing self-process uptime: %+v", self)
	}
}

func TestServerUptimeRejectsStaleCacheButSurvivesGatewayAdoption(t *testing.T) {
	now := time.Now()
	const fakePID = 999999
	m := &Manager{source: Source{PID: func() int { return fakePID }}}
	cache := newCollection()
	cache.PID, cache.At = fakePID, now
	cache.Health["jvm_process"] = health{Expected: true, Success: true}
	cache.Points["jvm_process"] = []point{gauge("mcs_jvm_process_start_time_seconds", "test", unix(now.Add(-2*time.Hour)))}
	s := Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true, StartedAt: now.Add(-time.Second)}}
	values := timingValues(m.points(Config{}, cache, s, rcon.Stats{}))
	if value, ok := values["mcs_minecraft_uptime_seconds"]; !ok || value < 7200 || value > 7201 {
		t.Fatalf("gateway adoption reset Java uptime: %+v", values)
	}
	checks := []struct {
		name   string
		change func(*collection, *Snapshot)
	}{
		{"different PID", func(c *collection, _ *Snapshot) { c.PID++ }},
		{"asleep", func(_ *collection, s *Snapshot) {
			s.Lifecycle.State, s.Lifecycle.BackendRunning = control.Asleep, false
		}},
		{"unknown service", func(_ *collection, s *Snapshot) { s.Lifecycle.BackendKnown = false }},
		{"before new boot", func(c *collection, s *Snapshot) { c.At = s.Lifecycle.StartedAt.Add(-time.Second) }},
		{"failed collector", func(c *collection, _ *Snapshot) { c.Health["jvm_process"] = health{Expected: true, Success: false} }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			c, changed := cache.clone(), s
			check.change(&c, &changed)
			values := timingValues(m.points(Config{}, c, changed, rcon.Stats{}))
			for _, name := range []string{"mcs_minecraft_uptime_seconds", "mcs_minecraft_start_time_seconds"} {
				if _, ok := values[name]; ok {
					t.Fatalf("stale state published %s", name)
				}
			}
		})
	}
}
