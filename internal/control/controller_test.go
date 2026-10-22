package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu          sync.Mutex
	running     bool
	ready       bool
	players     int
	starts      int
	stops       int
	checks      int
	runningErr  error
	playersErr  error
	statusErr   error
	startErr    error
	stopErr     error
	keepRunning bool
	playersHook func(context.Context) (int, error)
	runningHook func(context.Context) (bool, error)
	startHook   func(context.Context) error
	stopHook    func(context.Context) error
}

func (b *fakeBackend) Start(ctx context.Context) error {
	b.mu.Lock()
	b.starts++
	hook, err := b.startHook, b.startErr
	if hook == nil && err == nil {
		b.running = true
	}
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return err
}

func (b *fakeBackend) Running(ctx context.Context) (bool, error) {
	b.mu.Lock()
	hook, running, err := b.runningHook, b.running, b.runningErr
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return running, err
}

func (b *fakeBackend) Players(ctx context.Context) (int, error) {
	b.mu.Lock()
	b.checks++
	hook, n, err := b.playersHook, b.players, b.playersErr
	if err == nil && (!b.running || !b.ready) {
		err = errors.New("not ready")
	}
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return n, err
}

func (b *fakeBackend) Stop(ctx context.Context) error {
	b.mu.Lock()
	b.stops++
	hook, err := b.stopHook, b.stopErr
	if hook == nil && err == nil && !b.keepRunning {
		b.running = false
	}
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return err
}

func (b *fakeBackend) Status(context.Context) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return json.RawMessage(`{"version":{"protocol":767,"name":"1.21.1"},"players":{"online":0,"max":5},"betterStatus":{"opaque":[1,2]},"isModded":true}`), b.statusErr
}

func (b *fakeBackend) counts() (int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts, b.stops, b.checks
}

func testConfig() Config {
	return Config{
		IdleTimeout: time.Hour, PollInterval: 2 * time.Millisecond,
		OperationTimeout: 100 * time.Millisecond, StartupTimeout: time.Hour,
		ShutdownWarnAfter: time.Hour, WakeCooldown: time.Millisecond,
	}
}

func newTest(cfg Config, backend Backend) *Controller {
	return New(cfg, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func runTest(t *testing.T, c *Controller) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(time.Second):
			t.Error("controller did not exit after cancellation")
		}
	})
	eventually(t, func() bool { return c.started.Load() })
	return cancel
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before deadline")
}

func readyController(t *testing.T) (*Controller, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{running: true, ready: true}
	c := newTest(testConfig(), b)
	c.poll(context.Background())
	if c.Snapshot().State != Ready {
		t.Fatal("failed to adopt healthy backend")
	}
	return c, b
}

func TestAdoptsExistingBackendWithoutStart(t *testing.T) {
	c, b := readyController(t)
	if err := c.wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	starts, stops, _ := b.counts()
	if starts != 0 || stops != 0 || c.Snapshot().State != Ready {
		t.Fatalf("adoption changed lifecycle: starts=%d stops=%d snapshot=%+v", starts, stops, c.Snapshot())
	}
}

func TestConcurrentWakeStartsExactlyOnce(t *testing.T) {
	b := &fakeBackend{ready: true}
	c := newTest(testConfig(), b)
	runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Asleep })
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Wake(ctx); err != nil {
				t.Errorf("wake: %v", err)
			}
		}()
	}
	wg.Wait()
	eventually(t, func() bool { return c.Snapshot().State == Ready })
	starts, _, _ := b.counts()
	if starts != 1 {
		t.Fatalf("got %d starts; wanted one", starts)
	}
}

func TestLeaseBlocksShutdownAndReleaseIsIdempotent(t *testing.T) {
	c, b := readyController(t)
	release, ok := c.AcquireJoin()
	if !ok {
		t.Fatal("ready backend refused join lease")
	}
	if err := c.sleep(context.Background(), false); !errors.Is(err, ErrBusy) {
		t.Fatalf("sleep with pending join: %v", err)
	}
	release()
	release()
	if c.Snapshot().ActiveConnections != 0 {
		t.Fatal("idempotent release changed count incorrectly")
	}
	if err := c.sleep(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	c.poll(context.Background())
	_, stops, _ := b.counts()
	if stops != 1 || c.Snapshot().State != Asleep {
		t.Fatalf("expected one completed graceful stop: stops=%d snapshot=%+v", stops, c.Snapshot())
	}
}

func TestFinalPlayerCheckExcludesNewJoin(t *testing.T) {
	c, b := readyController(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	b.mu.Lock()
	b.playersHook = func(ctx context.Context) (int, error) {
		close(entered)
		select {
		case <-finish:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	b.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- c.sleep(context.Background(), false) }()
	<-entered
	if release, ok := c.AcquireJoin(); ok {
		release()
		t.Fatal("join entered after final shutdown check began")
	}
	if c.Snapshot().State != Stopping {
		t.Fatal("gate was not closed during final check")
	}
	close(finish)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	_, stops, _ := b.counts()
	if stops != 1 {
		t.Fatalf("expected one stop, got %d", stops)
	}
}

func TestPlayerAppearingAtFinalCheckCancelsShutdown(t *testing.T) {
	c, b := readyController(t)
	b.mu.Lock()
	b.players = 1
	b.mu.Unlock()
	if err := c.sleep(context.Background(), false); !errors.Is(err, ErrBusy) {
		t.Fatalf("sleep when player appeared: %v", err)
	}
	_, stops, _ := b.counts()
	s := c.Snapshot()
	if stops != 0 || s.State != Ready || s.Players == nil || *s.Players != 1 {
		t.Fatalf("unsafe final check: stops=%d state=%+v", stops, s)
	}
}

func TestRCONFailureNeverMeansEmptyAndResetsIdle(t *testing.T) {
	c, b := readyController(t)
	c.mu.Lock()
	c.snap.IdleSince = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	b.mu.Lock()
	b.playersErr = errors.New("RCON timed out")
	b.mu.Unlock()
	for range 5 {
		c.poll(context.Background())
	}
	s := c.Snapshot()
	if s.State != Unknown || s.Players != nil || !s.IdleSince.IsZero() {
		t.Fatalf("failed check retained unsafe emptiness: %+v", s)
	}
	if err := c.sleep(context.Background(), false); !errors.Is(err, ErrUnknownPlayers) {
		t.Fatalf("manual sleep accepted unknown count: %v", err)
	}
	b.mu.Lock()
	b.playersErr = nil
	b.mu.Unlock()
	recovered := time.Now()
	c.poll(context.Background())
	s = c.Snapshot()
	_, stops, _ := b.counts()
	if stops != 0 || s.State != Ready || s.IdleSince.Before(recovered) {
		t.Fatalf("recovery reused pre-failure idle window: stops=%d snapshot=%+v", stops, s)
	}
}

func TestFailedFinalRecheckDoesNotSendStop(t *testing.T) {
	c, b := readyController(t)
	b.mu.Lock()
	b.playersErr = errors.New("authentication failed")
	b.mu.Unlock()
	if err := c.sleep(context.Background(), false); err == nil {
		t.Fatal("failed final check returned success")
	}
	_, stops, _ := b.counts()
	if stops != 0 || c.Snapshot().State != Unknown {
		t.Fatalf("failed final check stopped backend: stops=%d snapshot=%+v", stops, c.Snapshot())
	}
}

func TestUncertainStopWaitsForActualExit(t *testing.T) {
	c, b := readyController(t)
	b.mu.Lock()
	b.stopErr = io.EOF
	b.mu.Unlock()
	if err := c.sleep(context.Background(), false); err == nil {
		t.Fatal("uncertain stop returned success")
	}
	for range 3 {
		c.poll(context.Background())
	}
	if err := c.wake(context.Background()); !errors.Is(err, ErrStopping) {
		t.Fatalf("wake accepted during uncertain shutdown: %v", err)
	}
	starts, stops, _ := b.counts()
	if starts != 0 || stops != 1 || c.Snapshot().State != Stopping {
		t.Fatalf("uncertain stop retried or guessed exit: starts=%d stops=%d snapshot=%+v", starts, stops, c.Snapshot())
	}
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	c.poll(context.Background())
	if c.Snapshot().State != Asleep {
		t.Fatal("actual exit did not complete shutdown")
	}
}

func TestShutdownWarningDoesNotKillOrRetry(t *testing.T) {
	c, b := readyController(t)
	b.mu.Lock()
	b.keepRunning = true
	b.mu.Unlock()
	if err := c.sleep(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.snap.StoppingSince = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	for range 3 {
		c.poll(context.Background())
	}
	_, stops, _ := b.counts()
	if stops != 1 || c.Snapshot().State != Stopping || c.Snapshot().LastError == "" {
		t.Fatalf("shutdown warning did not preserve running process: stops=%d snapshot=%+v", stops, c.Snapshot())
	}
}

func TestStartupTimeoutNeverLaunchesDuplicate(t *testing.T) {
	b := &fakeBackend{running: true, ready: false}
	c := newTest(testConfig(), b)
	c.poll(context.Background())
	c.mu.Lock()
	c.snap.StartedAt = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	c.poll(context.Background())
	if c.Snapshot().State != Failed {
		t.Fatalf("wanted stalled startup failure, got %+v", c.Snapshot())
	}
	if err := c.wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	starts, stops, _ := b.counts()
	if starts != 0 || stops != 0 {
		t.Fatalf("stalled process was duplicated or stopped: starts=%d stops=%d", starts, stops)
	}
	b.mu.Lock()
	b.ready = true
	b.mu.Unlock()
	c.poll(context.Background())
	if c.Snapshot().State != Ready {
		t.Fatal("late successful startup was not adopted")
	}
}

func TestIdleRequiresContinuousVerifiedZeroAndNoConnections(t *testing.T) {
	c, b := readyController(t)
	c.mu.Lock()
	c.snap.IdleSince = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	release, ok := c.AcquireJoin()
	if !ok {
		t.Fatal("lease refused")
	}
	c.poll(context.Background())
	_, stops, _ := b.counts()
	if stops != 0 || !c.Snapshot().IdleSince.IsZero() {
		t.Fatal("pending connection did not prevent idle shutdown")
	}
	release()
	c.poll(context.Background())
	if c.Snapshot().IdleSince.IsZero() {
		t.Fatal("new empty window did not start after release")
	}
	c.mu.Lock()
	c.snap.IdleSince = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	c.poll(context.Background())
	_, stops, _ = b.counts()
	if stops != 1 {
		t.Fatalf("verified expiry did not send one stop: %d", stops)
	}
}

func TestUnknownSupervisorNeverStartsBackend(t *testing.T) {
	b := &fakeBackend{runningErr: errors.New("systemd unavailable")}
	c := newTest(testConfig(), b)
	c.poll(context.Background())
	if err := c.wake(context.Background()); err == nil {
		t.Fatal("wake guessed backend state")
	}
	starts, _, _ := b.counts()
	if starts != 0 || c.Snapshot().BackendKnown {
		t.Fatalf("started under unknown supervisor state: starts=%d snapshot=%+v", starts, c.Snapshot())
	}
}

func TestCrashCooldownAndNoAutomaticRestart(t *testing.T) {
	c, b := readyController(t)
	c.cfg.WakeCooldown = time.Hour
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	c.poll(context.Background())
	first := c.Snapshot().NextWakeAt
	c.poll(context.Background())
	if !c.Snapshot().NextWakeAt.Equal(first) {
		t.Fatal("poll extended crash cooldown indefinitely")
	}
	if err := c.wake(context.Background()); !errors.Is(err, ErrCooldown) {
		t.Fatalf("crash cooldown not enforced: %v", err)
	}
	starts, _, _ := b.counts()
	if starts != 0 {
		t.Fatal("unexpected automatic restart")
	}
	c.mu.Lock()
	c.snap.NextWakeAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if err := c.wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	starts, _, _ = b.counts()
	if starts != 1 {
		t.Fatal("explicit wake after cooldown did not start")
	}
}

func TestCancellationLeavesBackendAlone(t *testing.T) {
	b := &fakeBackend{running: true, ready: true}
	c := newTest(testConfig(), b)
	cancel := runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Ready })
	cancel()
	<-c.done
	starts, stops, _ := b.counts()
	if starts != 0 || stops != 0 {
		t.Fatalf("controller cancellation changed server: starts=%d stops=%d", starts, stops)
	}
	if err := c.Wake(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("wake after controller exit: %v", err)
	}
}

func TestClientCancellationCannotCancelAcceptedBoot(t *testing.T) {
	b := &fakeBackend{ready: true}
	entered, finish := make(chan struct{}), make(chan struct{})
	b.startHook = func(ctx context.Context) error {
		close(entered)
		select {
		case <-finish:
			b.mu.Lock()
			b.running = true
			b.mu.Unlock()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cfg := testConfig()
	cfg.OperationTimeout = time.Second
	c := newTest(cfg, b)
	runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Asleep })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.Wake(ctx) }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected caller cancellation: %v", err)
	}
	close(finish)
	eventually(t, func() bool { return c.Snapshot().State == Ready })
	starts, stops, _ := b.counts()
	if starts != 1 || stops != 0 {
		t.Fatalf("cancelled client interrupted boot: starts=%d stops=%d", starts, stops)
	}
}

func TestCancelledSleepCallerDoesNotGuessExit(t *testing.T) {
	b := &fakeBackend{running: true, ready: true, keepRunning: true}
	entered, finish := make(chan struct{}), make(chan struct{})
	b.stopHook = func(ctx context.Context) error {
		close(entered)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cfg := testConfig()
	cfg.OperationTimeout = time.Second
	c := newTest(cfg, b)
	runTest(t, c)
	eventually(t, func() bool { return c.Snapshot().State == Ready })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.Sleep(ctx) }()
	<-entered
	cancel()
	<-result
	close(finish)
	eventually(t, func() bool { return c.Snapshot().State == Stopping })
	if release, ok := c.AcquireJoin(); ok {
		release()
		t.Fatal("cancelled caller reopened gate during shutdown")
	}
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	eventually(t, func() bool { return c.Snapshot().State == Asleep })
}

func TestCachedStatusAndSnapshotsAreCopies(t *testing.T) {
	c, _ := readyController(t)
	raw := json.RawMessage(`{"isModded":true,"betterStatus":{"opaque":123}}`)
	want := append([]byte(nil), raw...)
	if err := c.SetCachedStatus(raw); err != nil {
		t.Fatal(err)
	}
	raw[1] = 'X'
	got := c.CachedStatus()
	if !bytes.Equal(got, want) {
		t.Fatal("cache aliases caller input or modified opaque fields")
	}
	got[1] = 'Y'
	if !bytes.Equal(c.CachedStatus(), want) {
		t.Fatal("cache aliases returned slice")
	}
	s := c.Snapshot()
	*s.Players = 123
	if *c.Snapshot().Players != 0 {
		t.Fatal("snapshot aliases internal count")
	}
	for _, invalid := range []string{`null`, `[]`, `false`, `{`} {
		if err := c.SetCachedStatus(json.RawMessage(invalid)); err == nil {
			t.Errorf("accepted invalid status %q", invalid)
		}
	}
}

func TestRepeatedHealthErrorsAndHealthyPollsDoNotSpamLogs(t *testing.T) {
	var log bytes.Buffer
	b := &fakeBackend{running: true, ready: true}
	c := New(testConfig(), b, slog.New(slog.NewJSONHandler(&log, nil)))
	c.poll(context.Background())
	log.Reset()
	for range 10 {
		c.poll(context.Background())
	}
	if log.Len() != 0 {
		t.Fatalf("healthy polls wrote logs: %s", log.String())
	}
	b.mu.Lock()
	b.playersErr = errors.New("RCON timed out")
	b.mu.Unlock()
	for range 10 {
		c.poll(context.Background())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_error"`); got != 1 {
		t.Fatalf("wanted one health warning, got %d: %s", got, log.String())
	}
	log.Reset()
	b.mu.Lock()
	b.playersErr = nil
	b.mu.Unlock()
	for range 10 {
		c.poll(context.Background())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_recovered"`); got != 1 {
		t.Fatalf("wanted one recovery message, got %d: %s", got, log.String())
	}
}

func networkProbeError(port int, operation string, cause error) error {
	return fmt.Errorf("RCON command response incomplete: %w", &net.OpError{
		Op: operation, Net: "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		Addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 25575},
		Err:    cause,
	})
}

func TestNetworkErrorDedupIgnoresEphemeralPortsButKeepsLatestDiagnostic(t *testing.T) {
	var log bytes.Buffer
	b := &fakeBackend{running: true, ready: true}
	c := New(testConfig(), b, slog.New(slog.NewJSONHandler(&log, nil)))
	c.poll(context.Background())
	log.Reset()
	for port := 40001; port <= 40003; port++ {
		b.mu.Lock()
		b.playersErr = networkProbeError(port, "read", os.ErrDeadlineExceeded)
		b.mu.Unlock()
		c.poll(context.Background())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_error"`); got != 1 {
		t.Fatalf("one outage with changing client ports logged %d warnings: %s", got, log.String())
	}
	if !strings.Contains(c.Snapshot().LastError, "40003") {
		t.Fatalf("snapshot lost latest full diagnostic: %q", c.Snapshot().LastError)
	}
	if !strings.Contains(log.String(), "40001") || !strings.Contains(log.String(), "25575") {
		t.Fatalf("first warning lost useful connection diagnostic: %s", log.String())
	}
	// A different cause or operation is useful new information and is logged.
	for _, err := range []error{
		networkProbeError(40004, "read", io.ErrUnexpectedEOF),
		networkProbeError(40005, "write", io.ErrUnexpectedEOF),
	} {
		b.mu.Lock()
		b.playersErr = err
		b.mu.Unlock()
		c.poll(context.Background())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_error"`); got != 3 {
		t.Fatalf("changed error cause/operation should be logged; got %d: %s", got, log.String())
	}
	b.mu.Lock()
	b.playersErr = nil
	b.mu.Unlock()
	c.poll(context.Background())
	b.mu.Lock()
	b.playersErr = networkProbeError(40006, "write", io.ErrUnexpectedEOF)
	b.mu.Unlock()
	c.poll(context.Background())
	if got := strings.Count(log.String(), `"msg":"backend_health_recovered"`); got != 1 {
		t.Fatalf("wanted one recovery, got %d: %s", got, log.String())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_error"`); got != 4 {
		t.Fatalf("new outage after recovery was suppressed; got %d: %s", got, log.String())
	}
}

func TestInitialReadinessAndStalledStartupLogsStayClear(t *testing.T) {
	var log bytes.Buffer
	b := &fakeBackend{running: true, ready: false}
	c := New(testConfig(), b, slog.New(slog.NewJSONHandler(&log, nil)))
	for port := 41001; port <= 41003; port++ {
		b.mu.Lock()
		b.playersErr = networkProbeError(port, "read", os.ErrDeadlineExceeded)
		b.mu.Unlock()
		c.poll(context.Background())
	}
	if c.Snapshot().State != Starting || c.Snapshot().Players != nil {
		t.Fatalf("unready backend should be starting with unknown players: %+v", c.Snapshot())
	}
	if strings.Count(log.String(), `"msg":"state_changed"`) != 1 || strings.Contains(log.String(), `"msg":"backend_health_error"`) {
		t.Fatalf("ordinary boot should log one state transition without repeated health warnings: %s", log.String())
	}
	c.mu.Lock()
	c.snap.StartedAt = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	for port := 41004; port <= 41006; port++ {
		b.mu.Lock()
		b.playersErr = networkProbeError(port, "read", os.ErrDeadlineExceeded)
		b.mu.Unlock()
		c.poll(context.Background())
	}
	if c.Snapshot().State != Failed || !strings.Contains(c.Snapshot().LastError, "41006") {
		t.Fatalf("stalled startup lost state or latest diagnostic: %+v", c.Snapshot())
	}
	if got := strings.Count(log.String(), `"msg":"backend_health_error"`); got != 1 {
		t.Fatalf("stalled startup with changed ports logged %d warnings: %s", got, log.String())
	}
	b.mu.Lock()
	b.ready, b.playersErr = true, nil
	b.mu.Unlock()
	c.poll(context.Background())
	if c.Snapshot().State != Ready || strings.Count(log.String(), `"msg":"backend_health_recovered"`) != 1 {
		t.Fatalf("late readiness did not log clear recovery: state=%+v logs=%s", c.Snapshot(), log.String())
	}
}
