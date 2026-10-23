package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReconfigurePreservesIdleClockAndResetsPolling(t *testing.T) {
	backend := &fakeBackend{running: true, ready: true}
	cfg := testConfig()
	cfg.PollInterval = time.Hour
	c := newTest(cfg, backend)
	cancel := runTest(t, c)
	eventually(t, func() bool { return !c.Snapshot().IdleSince.IsZero() })
	before := c.Snapshot()
	_, _, initialChecks := backend.counts()
	updated := cfg
	updated.IdleTimeout = 2 * time.Hour
	updated.PollInterval = 2 * time.Millisecond
	updated.OperationTimeout = time.Second
	updated.StartupTimeout = 20 * time.Minute
	updated.ShutdownWarnAfter = 2 * time.Minute
	updated.WakeCooldown = 5 * time.Second
	if err := c.Reconfigure(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		_, _, checks := backend.counts()
		return checks >= initialChecks+3
	})
	after := c.Snapshot()
	if after.State != Ready || after.Players == nil || *after.Players != 0 || !after.IdleSince.Equal(before.IdleSince) || !after.StartedAt.Equal(before.StartedAt) || !after.NextWakeAt.Equal(before.NextWakeAt) {
		t.Fatalf("reload changed existing lifecycle state: before=%+v after=%+v", before, after)
	}
	starts, stops, _ := backend.counts()
	if starts != 0 || stops != 0 {
		t.Fatalf("reload changed process state: starts=%d stops=%d", starts, stops)
	}
	cancel()
	<-c.done
	if c.cfg != updated {
		t.Fatalf("not all configuration fields were applied: got %+v want %+v", c.cfg, updated)
	}
}

func TestReconfigurePreservesPlayersAndJoinLeases(t *testing.T) {
	for _, occupiedBy := range []string{"players", "join lease"} {
		t.Run(occupiedBy, func(t *testing.T) {
			backend := &fakeBackend{running: true, ready: true}
			if occupiedBy == "players" {
				backend.players = 1
			}
			cfg := testConfig()
			c := newTest(cfg, backend)
			runTest(t, c)
			eventually(t, func() bool { return c.Snapshot().State == Ready })
			if occupiedBy == "join lease" {
				release, ok := c.AcquireJoin()
				if !ok {
					t.Fatal("could not reserve join")
				}
				defer release()
			}
			cfg.IdleTimeout = 5 * time.Millisecond
			cfg.PollInterval = time.Millisecond
			if err := c.Reconfigure(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			_, _, initialChecks := backend.counts()
			eventually(t, func() bool {
				_, _, checks := backend.counts()
				return checks >= initialChecks+20
			})
			snap := c.Snapshot()
			_, stops, _ := backend.counts()
			if stops != 0 || snap.State != Ready || !snap.IdleSince.IsZero() {
				t.Fatalf("occupied server was treated as idle after reload: stops=%d snapshot=%+v", stops, snap)
			}
			if occupiedBy == "players" && (snap.Players == nil || *snap.Players != 1) {
				t.Fatalf("player count lost: %+v", snap)
			}
			if occupiedBy == "join lease" && snap.ActiveConnections != 1 {
				t.Fatalf("join lease lost: %+v", snap)
			}
		})
	}
}

func TestShorterIdleTimeoutUsesExistingIdleClock(t *testing.T) {
	backend := &fakeBackend{running: true, ready: true}
	cfg := testConfig()
	cfg.PollInterval = time.Hour
	c := newTest(cfg, backend)
	runTest(t, c)
	eventually(t, func() bool { return !c.Snapshot().IdleSince.IsZero() })
	c.mu.Lock()
	c.snap.IdleSince = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	cfg.IdleTimeout = 30 * time.Second
	cfg.PollInterval = time.Millisecond
	if err := c.Reconfigure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		_, stops, _ := backend.counts()
		return stops == 1
	})
}

func TestReconfigureRejectsInvalidConfigurationWithoutMutation(t *testing.T) {
	cfg := testConfig()
	c := newTest(cfg, &fakeBackend{running: true, ready: true, players: 1})
	cancel := runTest(t, c)
	changes := []func(*Config){
		func(c *Config) { c.IdleTimeout = 0 },
		func(c *Config) { c.PollInterval = -time.Second },
		func(c *Config) { c.OperationTimeout = 0 },
		func(c *Config) { c.StartupTimeout = 0 },
		func(c *Config) { c.ShutdownWarnAfter = 0 },
		func(c *Config) { c.WakeCooldown = 0 },
		func(c *Config) { c.PollInterval = c.IdleTimeout + time.Second },
	}
	for i, change := range changes {
		invalid := cfg
		change(&invalid)
		if err := c.Reconfigure(context.Background(), invalid); err == nil {
			t.Errorf("invalid configuration %d was accepted", i)
		}
	}
	cancel()
	<-c.done
	if c.cfg != cfg {
		t.Fatalf("rejected update mutated configuration: %+v", c.cfg)
	}
	if err := c.Reconfigure(context.Background(), cfg); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped controller update: %v", err)
	}
}

func TestCancelledQueuedReconfigureCannotCommit(t *testing.T) {
	entered := make(chan struct{})
	continuePoll := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continuePoll) }) }
	defer release()
	backend := &fakeBackend{running: true, ready: true, players: 1}
	backend.runningHook = func(ctx context.Context) (bool, error) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-continuePoll:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	cfg := testConfig()
	cfg.OperationTimeout = time.Second
	c := newTest(cfg, backend)
	stop := runTest(t, c)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial poll did not begin")
	}
	updated := cfg
	updated.IdleTimeout = 2 * time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Reconfigure(ctx, updated) }()
	eventually(t, func() bool { return len(c.reqs) == 1 })
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled queued update returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued cancellation waited for network operation")
	}
	release()
	eventually(t, func() bool { return len(c.reqs) == 0 && c.Snapshot().State == Ready })
	stop()
	<-c.done
	if c.cfg != cfg {
		t.Fatalf("cancelled update committed: %+v", c.cfg)
	}
}

func TestConcurrentReconfigureAndJoinLeaseActivity(t *testing.T) {
	backend := &fakeBackend{running: true, ready: true, players: 1}
	cfg := testConfig()
	c := newTest(cfg, backend)
	runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Ready })
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				updated := cfg
				updated.IdleTimeout = time.Duration(worker+1) * time.Hour
				updated.PollInterval = time.Duration(i%3+1) * time.Millisecond
				if err := c.Reconfigure(context.Background(), updated); err != nil {
					t.Errorf("concurrent update: %v", err)
					return
				}
				if release, ok := c.AcquireJoin(); ok {
					_ = c.Snapshot()
					release()
				} else {
					t.Error("reload interrupted ready state")
					return
				}
			}
		}(worker)
	}
	workers.Wait()
	starts, stops, _ := backend.counts()
	if starts != 0 || stops != 0 || c.Snapshot().ActiveConnections != 0 {
		t.Fatalf("concurrent reload changed process or leases: starts=%d stops=%d snapshot=%+v", starts, stops, c.Snapshot())
	}
}

func TestReconfigureHookExcludesWakeAndAcknowledgesAfterCancellation(t *testing.T) {
	backend := &fakeBackend{ready: true}
	cfg := testConfig()
	c := newTest(cfg, backend)
	stop := runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Asleep })
	entered, releaseHook := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHook) }) }
	defer release()
	updated := cfg
	updated.WakeCooldown = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.ReconfigureWith(ctx, updated, func() error {
			close(entered)
			<-releaseHook
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("precommit hook was not accepted")
	}
	wakeResult := make(chan error, 1)
	go func() { wakeResult <- c.Wake(context.Background()) }()
	eventually(t, func() bool { return len(c.reqs) == 1 })
	cancel()
	select {
	case err := <-result:
		t.Fatalf("accepted update returned before hook finished: %v", err)
	default:
	}
	starts, _, _ := backend.counts()
	if starts != 0 {
		t.Fatal("wake ran concurrently with precommit hook")
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("committed update returned an uncertain error after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted update never acknowledged")
	}
	select {
	case err := <-wakeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serialized wake did not resume")
	}
	stop()
	<-c.done
	if c.cfg != updated {
		t.Fatal("acknowledged update was not applied")
	}
}

func TestFailedReconfigureHookLeavesConfigurationAndIdleStateUntouched(t *testing.T) {
	backend := &fakeBackend{running: true, ready: true}
	cfg := testConfig()
	cfg.PollInterval = time.Hour
	c := newTest(cfg, backend)
	stop := runTest(t, c)
	eventually(t, func() bool { return !c.Snapshot().IdleSince.IsZero() })
	before := c.Snapshot()
	updated := cfg
	updated.IdleTimeout = 2 * time.Hour
	failure := errors.New("related settings could not be committed")
	if err := c.ReconfigureWith(context.Background(), updated, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("hook error was not returned: %v", err)
	}
	after := c.Snapshot()
	if !after.IdleSince.Equal(before.IdleSince) || after.State != before.State {
		t.Fatalf("failed hook changed lifecycle state: before=%+v after=%+v", before, after)
	}
	stop()
	<-c.done
	if c.cfg != cfg {
		t.Fatal("failed hook changed configuration")
	}
}
