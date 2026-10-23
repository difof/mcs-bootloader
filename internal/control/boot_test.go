package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type identifiedTestBackend struct {
	*fakeBackend
	identity, nextIdentity string
	statusHook             func()
}

func (b *identifiedTestBackend) Status(ctx context.Context) (json.RawMessage, error) {
	data, err := b.fakeBackend.Status(ctx)
	if b.statusHook != nil {
		b.statusHook()
	}
	return data, err
}

func (b *identifiedTestBackend) Start(ctx context.Context) error {
	if err := b.fakeBackend.Start(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	b.identity = b.nextIdentity
	b.mu.Unlock()
	return nil
}

func (b *identifiedTestBackend) ProcessIdentity() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.identity
}

func bootTestController() (*Controller, *identifiedTestBackend, *time.Time) {
	b := &identifiedTestBackend{fakeBackend: &fakeBackend{}, nextIdentity: "first-invocation"}
	c := newTest(testConfig(), b)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.poll(context.Background())
	return c, b, &now
}

func TestBootHistoryLearnsSuccessfulStartsAndUsesAttemptClock(t *testing.T) {
	c, b, now := bootTestController()
	ctx := context.Background()
	if err := c.wake(ctx); err != nil {
		t.Fatal(err)
	}
	started := *now
	*now = now.Add(40 * time.Second)
	c.poll(ctx)
	if err := c.wake(ctx); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); !s.BootStartedAt.Equal(started) || s.SuccessfulBoots != 0 {
		t.Fatalf("retry reset boot timer or learned an unready boot: %+v", s)
	}
	*now = now.Add(20 * time.Second)
	b.ready = true
	c.poll(ctx)
	s := c.Snapshot()
	if s.State != Ready || s.SuccessfulBoots != 1 || s.LastBootDurationSeconds != 60 || s.BootEstimateSeconds != 60 || !s.ReadySince.Equal(*now) {
		t.Fatalf("incorrect first successful measurement: %+v", s)
	}
	*now = now.Add(time.Minute)
	c.poll(ctx)
	if c.Snapshot().SuccessfulBoots != 1 || !c.Snapshot().ReadySince.Equal(s.ReadySince) {
		t.Fatal("routine polling added boot observations or reset ready clock")
	}

	if err := c.sleep(ctx, false); err != nil {
		t.Fatal(err)
	}
	c.poll(ctx)
	if !c.Snapshot().BootStartedAt.IsZero() || !c.Snapshot().ReadySince.IsZero() {
		t.Fatal("stopped server retained active timing")
	}
	*now = now.Add(time.Second)
	b.ready, b.nextIdentity = false, "second-invocation"
	if err := c.wake(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(100 * time.Second)
	b.ready = true
	c.poll(ctx)
	s = c.Snapshot()
	if s.SuccessfulBoots != 2 || s.LastBootDurationSeconds != 100 || s.BootEstimateSeconds != 70 {
		t.Fatalf("second observation did not smooth estimate: %+v", s)
	}
	var cached map[string]any
	if err := json.Unmarshal(c.CachedBootHistory(), &cached); err != nil || len(cached) != 4 {
		t.Fatalf("cache should contain only version and completed observations: %v %+v", err, cached)
	}
}

func TestBootHistoryDoesNotLearnFailuresAdoptionOrReplacement(t *testing.T) {
	for _, scenario := range []string{"start_failed", "exited", "replaced", "unknown_identity", "adopted"} {
		t.Run(scenario, func(t *testing.T) {
			c, b, now := bootTestController()
			ctx := context.Background()
			if scenario == "start_failed" {
				b.startErr = errors.New("failed")
			}
			if scenario == "unknown_identity" {
				b.nextIdentity = ""
			}
			if scenario == "adopted" {
				b.running, b.identity = true, "independent"
				c.poll(ctx)
			} else if err := c.wake(ctx); err != nil && scenario != "start_failed" {
				t.Fatal(err)
			}
			*now = now.Add(time.Minute)
			switch scenario {
			case "start_failed", "exited":
				b.running = false
				c.poll(ctx)
				b.running, b.identity = true, "independent"
			case "replaced":
				b.identity = "independent"
			}
			b.ready = true
			c.poll(ctx)
			if s := c.Snapshot(); s.State != Ready || s.SuccessfulBoots != 0 || s.BootEstimateSeconds != 0 || len(c.CachedBootHistory()) != 0 {
				t.Fatalf("incomplete/independent process was learned: %+v", s)
			}
		})
	}
}

func TestBootHistoryRechecksProcessAfterReadinessProbe(t *testing.T) {
	for _, scenario := range []string{"replaced", "exited", "inspect_failed"} {
		t.Run(scenario, func(t *testing.T) {
			c, b, now := bootTestController()
			ctx := context.Background()
			if err := c.wake(ctx); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Minute)
			b.ready = true
			b.statusHook = func() {
				switch scenario {
				case "replaced":
					b.identity = "replacement-during-probe"
				case "exited":
					b.running = false
				case "inspect_failed":
					b.runningErr = errors.New("systemd unavailable")
				}
			}
			c.poll(ctx)
			if s := c.Snapshot(); s.State == Ready || s.SuccessfulBoots != 0 {
				t.Fatalf("raced readiness probe was learned/admitted: %+v", s)
			}
			b.statusHook, b.runningErr, b.running = nil, nil, true
			c.poll(ctx)
			if s := c.Snapshot(); s.State != Ready || s.SuccessfulBoots != 0 {
				t.Fatalf("subsequent adoption should be healthy without learning: %+v", s)
			}
		})
	}
}

func TestBootHistorySurvivesGatewayRestartWithoutLearningAdoption(t *testing.T) {
	c, b, now := bootTestController()
	if err := c.wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(75 * time.Second)
	b.ready = true
	c.poll(context.Background())
	cache := c.CachedBootHistory()
	restarted := newTest(testConfig(), b)
	restarted.now = c.now
	if err := restarted.RestoreBootHistory(cache); err != nil {
		t.Fatal(err)
	}
	restarted.poll(context.Background())
	s := restarted.Snapshot()
	if s.State != Ready || s.SuccessfulBoots != 1 || s.BootEstimateSeconds != 75 || !s.BootStartedAt.IsZero() || !bytes.Equal(cache, restarted.CachedBootHistory()) {
		t.Fatalf("gateway adoption modified completed history or restored active timer: %+v", s)
	}
	// An outage followed by recovery opens a fresh observed-ready interval,
	// but is never a second successful Minecraft startup.
	*now = now.Add(time.Minute)
	b.ready = false
	restarted.poll(context.Background())
	if !restarted.Snapshot().ReadySince.IsZero() {
		t.Fatal("unhealthy backend retained ready interval")
	}
	*now = now.Add(time.Minute)
	b.ready = true
	restarted.poll(context.Background())
	s = restarted.Snapshot()
	if !s.ReadySince.Equal(*now) || s.SuccessfulBoots != 1 {
		t.Fatalf("recovery did not isolate readiness from boot history: %+v", s)
	}
}

func TestBootHistoryRejectsCorruptionWithoutPartialUpdate(t *testing.T) {
	valid := []byte(`{"version":1,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":4}`)
	c, _, _ := bootTestController()
	if err := c.RestoreBootHistory(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(`{`),
		[]byte(`{"version":2,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":4}`),
		[]byte(`{"version":1,"estimate_seconds":-1,"last_duration_seconds":70,"successful_boots":4}`),
		[]byte(`{"version":1,"estimate_seconds":1e309,"last_duration_seconds":70,"successful_boots":4}`),
		[]byte(`{"version":1,"estimate_seconds":86401,"last_duration_seconds":70,"successful_boots":4}`),
		[]byte(`{"version":1,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":0}`),
		[]byte(`{"version":1,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":1000000001}`),
		[]byte(`{"version":1,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":4,"extra":1}`),
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		bytes.Repeat([]byte(" "), 4097),
	} {
		if err := c.RestoreBootHistory(bad); err == nil {
			t.Fatalf("accepted corrupt cache %q", bad)
		}
		if !bytes.Equal(c.CachedBootHistory(), valid) {
			t.Fatal("failed restoration partially changed history")
		}
	}
}
