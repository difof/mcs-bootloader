// Package control coordinates a separately supervised Minecraft server. It
// never signals or kills that server: the only shutdown operation is RCON stop.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type State string

const (
	Unknown  State = "unknown"
	Asleep   State = "asleep"
	Starting State = "starting"
	Ready    State = "ready"
	Stopping State = "stopping"
	Failed   State = "failed"
)

var (
	ErrNotRunning     = errors.New("lifecycle controller is not running")
	ErrStopping       = errors.New("backend is stopping; retry after it exits")
	ErrBusy           = errors.New("players or connections are active")
	ErrUnknownPlayers = errors.New("player count is unknown; refusing shutdown")
	ErrCooldown       = errors.New("wake cooldown is active")
	ErrQueueFull      = errors.New("lifecycle request queue is full")
)

// Backend must honor context cancellation. Running reports the process/service
// state, not whether a network port happens to accept connections. Players must
// return an error for a failed or unrecognized response, never a guessed zero.
type Backend interface {
	Start(context.Context) error
	Running(context.Context) (bool, error)
	Players(context.Context) (int, error)
	Stop(context.Context) error
	Status(context.Context) (json.RawMessage, error)
}

type Config struct {
	IdleTimeout       time.Duration
	PollInterval      time.Duration
	OperationTimeout  time.Duration
	StartupTimeout    time.Duration
	ShutdownWarnAfter time.Duration
	WakeCooldown      time.Duration
}

// Validate checks a complete runtime configuration. Reconfiguration never
// substitutes defaults for missing values, so a bad reload cannot silently
// shorten a safety timeout.
func (cfg Config) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"idle timeout", cfg.IdleTimeout},
		{"poll interval", cfg.PollInterval},
		{"operation timeout", cfg.OperationTimeout},
		{"startup timeout", cfg.StartupTimeout},
		{"shutdown warning interval", cfg.ShutdownWarnAfter},
		{"wake cooldown", cfg.WakeCooldown},
	} {
		if field.value <= 0 {
			return fmt.Errorf("%s must be positive", field.name)
		}
	}
	if cfg.PollInterval > cfg.IdleTimeout {
		return errors.New("poll interval must not exceed idle timeout")
	}
	return nil
}

type Snapshot struct {
	State               State     `json:"state"`
	Players             *int      `json:"players"`
	ActiveConnections   int       `json:"active_connections"`
	BackendRunning      bool      `json:"backend_running"`
	BackendKnown        bool      `json:"backend_known"`
	IdleSince           time.Time `json:"idle_since,omitempty"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	StoppingSince       time.Time `json:"stopping_since,omitempty"`
	LastCheck           time.Time `json:"last_check,omitempty"`
	LastSuccessfulCheck time.Time `json:"last_successful_check,omitempty"`
	NextWakeAt          time.Time `json:"next_wake_at,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
}

type request struct {
	ctx           context.Context
	sleep         bool
	reply         chan error
	configuration *configurationRequest
}

type configurationRequest struct {
	cfg          Config
	beforeCommit func() error
	// 0: queued; 1: accepted by the worker; 2: cancelled before acceptance.
	// The caller and worker race only for acceptance, never over cfg itself.
	phase atomic.Uint32
}

// Controller has one worker for backend operations. Its small mutex protects
// snapshots and join leases; no network or process operation holds that mutex.
type Controller struct {
	cfg     Config
	backend Backend
	logger  *slog.Logger
	mu      sync.RWMutex
	snap    Snapshot
	status  json.RawMessage
	reqs    chan request
	done    chan struct{}
	started atomic.Bool
	// The following fields belong exclusively to Run's worker.
	wasReady      bool
	reportedError string
}

func New(cfg Config, backend Backend, logger *slog.Logger) *Controller {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Hour
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 5 * time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 15 * time.Minute
	}
	if cfg.ShutdownWarnAfter <= 0 {
		cfg.ShutdownWarnAfter = 5 * time.Minute
	}
	if cfg.WakeCooldown <= 0 {
		cfg.WakeCooldown = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		cfg: cfg, backend: backend, logger: logger,
		snap: Snapshot{State: Unknown}, reqs: make(chan request, 64), done: make(chan struct{}),
	}
}

// Run is called once, normally in a goroutine. Cancelling it only stops the
// controller; a running backend is deliberately left under its supervisor.
func (c *Controller) Run(ctx context.Context) {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	defer close(c.done)
	c.poll(ctx)
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-c.reqs:
			if update := r.configuration; update != nil {
				if err := r.ctx.Err(); err != nil {
					update.phase.CompareAndSwap(0, 2)
					r.reply <- err
					continue
				}
				if !update.phase.CompareAndSwap(0, 1) {
					r.reply <- r.ctx.Err()
					continue
				}
				if update.beforeCommit != nil {
					if err := update.beforeCommit(); err != nil {
						r.reply <- err
						continue
					}
				}
				// A configuration commit has no external I/O. Preserve state,
				// leases, and the existing idle timestamp across the change.
				pollChanged := c.cfg.PollInterval != update.cfg.PollInterval
				c.cfg = update.cfg
				if pollChanged {
					ticker.Reset(c.cfg.PollInterval)
				}
				r.reply <- nil
				continue
			}
			if err := r.ctx.Err(); err != nil {
				r.reply <- err
				continue
			}
			// Once accepted, the operation belongs to the controller's lifetime,
			// not the connecting client's. Disconnects cannot cancel a boot.
			if r.sleep {
				r.reply <- c.sleep(ctx, false)
			} else {
				r.reply <- c.wake(ctx)
			}
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Controller) Wake(ctx context.Context) error  { return c.submit(ctx, false) }
func (c *Controller) Sleep(ctx context.Context) error { return c.submit(ctx, true) }

// Reconfigure serializes a validated update with all lifecycle operations.
// Cancellation before worker acceptance prevents the update. Once accepted,
// this call waits for its acknowledgement even if ctx expires, so an error
// never leaves callers guessing whether configuration was committed.
func (c *Controller) Reconfigure(ctx context.Context, cfg Config) error {
	return c.ReconfigureWith(ctx, cfg, nil)
}

// ReconfigureWith runs a precommit hook on the lifecycle worker, excluding
// wake, sleep, and poll operations while related runtime settings are updated.
// The hook must use bounded I/O, must not call back into the controller's
// worker, and must leave its own state unchanged if it returns an error.
// Once accepted, cancellation waits for the hook and definitive commit result.
func (c *Controller) ReconfigureWith(ctx context.Context, cfg Config, beforeCommit func() error) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.started.Load() {
		return ErrNotRunning
	}
	update := &configurationRequest{cfg: cfg, beforeCommit: beforeCommit}
	r := request{ctx: ctx, reply: make(chan error, 1), configuration: update}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrNotRunning
	case c.reqs <- r:
	default:
		return ErrQueueFull
	}
	for {
		select {
		case err := <-r.reply:
			return err
		case <-ctx.Done():
			if update.phase.CompareAndSwap(0, 2) {
				return ctx.Err()
			}
			// An accepted commit is synchronous on the worker. Its buffered
			// acknowledgement is delivered before the worker can exit.
			return <-r.reply
		case <-c.done:
			select {
			case err := <-r.reply:
				return err
			default:
				return ErrNotRunning
			}
		}
	}
}

func (c *Controller) submit(ctx context.Context, sleep bool) error {
	if !c.started.Load() {
		return ErrNotRunning
	}
	r := request{ctx: ctx, sleep: sleep, reply: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrNotRunning
	case c.reqs <- r:
	default:
		return ErrQueueFull
	}
	select {
	case err := <-r.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrNotRunning
	}
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snap
	if s.Players != nil {
		n := *s.Players
		s.Players = &n
	}
	return s
}

// AcquireJoin reserves a real connection before the gateway dials the backend.
// The reservation closes the otherwise dangerous player-check/join race.
func (c *Controller) AcquireJoin() (release func(), ok bool) {
	c.mu.Lock()
	if c.snap.State != Ready {
		c.mu.Unlock()
		return nil, false
	}
	c.snap.ActiveConnections++
	c.snap.IdleSince = time.Time{}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.snap.ActiveConnections--
			// A fresh successful RCON poll must establish emptiness again.
			c.snap.IdleSince = time.Time{}
			c.mu.Unlock()
		})
	}, true
}

func (c *Controller) CachedStatus() json.RawMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append(json.RawMessage(nil), c.status...)
}

// SetCachedStatus accepts only an object. Unknown/mod-provided fields remain
// opaque, and both input and output are copied to avoid shared mutable slices.
func (c *Controller) SetCachedStatus(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return errors.New("server status must be a JSON object")
	}
	c.mu.Lock()
	c.status = append(json.RawMessage(nil), raw...)
	c.mu.Unlock()
	return nil
}

func (c *Controller) wake(ctx context.Context) error {
	s := c.Snapshot()
	if s.State == Stopping {
		return ErrStopping
	}
	op, cancel := context.WithTimeout(ctx, c.cfg.OperationTimeout)
	defer cancel()
	running, err := c.backend.Running(op)
	if err != nil {
		c.unhealthy(Unknown, "cannot determine backend state", err, true)
		return fmt.Errorf("check backend before wake: %w", err)
	}
	if running {
		// Includes a server that is still booting after a warning timeout, or
		// one started independently while the gateway was asleep.
		if s.State == Asleep {
			c.wasReady = false
			c.update(Starting, "adopting running backend", func(s *Snapshot) {
				s.BackendRunning, s.BackendKnown = true, true
				s.StartedAt = time.Now()
				s.Players, s.IdleSince = nil, time.Time{}
			})
		}
		return nil
	}
	if time.Now().Before(s.NextWakeAt) {
		return ErrCooldown
	}
	c.wasReady = false
	now := time.Now()
	c.update(Starting, "wake requested", func(s *Snapshot) {
		s.BackendRunning, s.BackendKnown = false, true
		s.Players, s.IdleSince = nil, time.Time{}
		s.StartedAt, s.NextWakeAt = now, now.Add(c.cfg.WakeCooldown)
		s.StoppingSince, s.LastError = time.Time{}, ""
	})
	if err := c.backend.Start(op); err != nil {
		c.unhealthy(Failed, "backend start failed", err, true)
		return fmt.Errorf("start backend: %w", err)
	}
	return nil
}

func (c *Controller) poll(ctx context.Context) {
	op, cancel := context.WithTimeout(ctx, c.cfg.OperationTimeout)
	defer cancel()
	s := c.Snapshot()
	running, err := c.backend.Running(op)
	if err != nil {
		state := Unknown
		if s.State == Stopping {
			state = Stopping
		}
		c.unhealthy(state, "cannot determine backend state", err, true)
		return
	}
	if !running {
		state, reason := Asleep, "backend is stopped"
		message := ""
		if s.State == Failed {
			state, message = Failed, s.LastError
		} else if s.State == Ready || s.State == Starting || (s.BackendRunning && s.State != Stopping) {
			state, reason, message = Failed, "backend exited unexpectedly", "backend exited unexpectedly"
		}
		c.update(state, reason, func(s *Snapshot) {
			s.BackendRunning, s.BackendKnown = false, true
			s.Players, s.IdleSince = nil, time.Time{}
			s.LastCheck, s.LastError = time.Now(), message
			if state == Failed && reason == "backend exited unexpectedly" {
				s.NextWakeAt = time.Now().Add(c.cfg.WakeCooldown)
			}
			if state == Asleep {
				s.StoppingSince = time.Time{}
			}
		})
		c.reportError(message)
		return
	}
	c.mu.Lock()
	c.snap.BackendRunning, c.snap.BackendKnown = true, true
	if c.snap.StartedAt.IsZero() || s.State == Asleep {
		c.snap.StartedAt = time.Now()
		c.wasReady = false
	}
	c.mu.Unlock()
	if s.State == Stopping {
		if !s.StoppingSince.IsZero() && time.Since(s.StoppingSince) >= c.cfg.ShutdownWarnAfter {
			msg := "graceful shutdown is taking longer than expected; waiting without sending signals"
			c.update(Stopping, "waiting for graceful shutdown", func(s *Snapshot) { s.LastError = msg })
			c.reportError(msg)
		}
		return
	}
	players, err := c.backend.Players(op)
	if err == nil && players < 0 {
		err = errors.New("backend returned a negative player count")
	}
	if err != nil {
		c.notReady(err)
		return
	}
	raw, err := c.backend.Status(op)
	if err == nil {
		err = c.SetCachedStatus(raw)
	}
	if err != nil {
		c.notReady(fmt.Errorf("server status: %w", err))
		return
	}
	c.wasReady = true
	now := time.Now()
	c.update(Ready, "backend ready", func(s *Snapshot) {
		s.Players = &players
		s.LastCheck, s.LastSuccessfulCheck, s.LastError = now, now, ""
		if players > 0 || s.ActiveConnections > 0 {
			s.IdleSince = time.Time{}
		} else if s.IdleSince.IsZero() {
			s.IdleSince = now
		}
	})
	c.reportError("")
	s = c.Snapshot()
	if !s.IdleSince.IsZero() && time.Since(s.IdleSince) >= c.cfg.IdleTimeout {
		_ = c.sleep(ctx, true)
	}
}

func (c *Controller) notReady(err error) {
	state, report := Starting, false
	s := c.Snapshot()
	if c.wasReady {
		state, report = Unknown, true
	} else if !s.StartedAt.IsZero() && time.Since(s.StartedAt) >= c.cfg.StartupTimeout {
		state, report = Failed, true
		err = fmt.Errorf("startup readiness timeout; backend left running: %w", err)
	}
	c.unhealthy(state, "backend not ready", err, report)
}

func (c *Controller) sleep(ctx context.Context, automatic bool) error {
	// Changing state before the final RCON check excludes new join leases.
	c.mu.Lock()
	s := c.snap
	if s.State == Asleep {
		c.mu.Unlock()
		return nil
	}
	if s.State == Stopping {
		c.mu.Unlock()
		return ErrStopping
	}
	if s.ActiveConnections != 0 || (s.Players != nil && *s.Players > 0) {
		c.mu.Unlock()
		return ErrBusy
	}
	if s.State != Ready || s.Players == nil {
		c.mu.Unlock()
		return ErrUnknownPlayers
	}
	if automatic && (s.IdleSince.IsZero() || time.Since(s.IdleSince) < c.cfg.IdleTimeout) {
		c.mu.Unlock()
		return ErrBusy
	}
	c.snap.State, c.snap.StoppingSince = Stopping, time.Now()
	c.snap.IdleSince = time.Time{}
	c.mu.Unlock()
	reason := "manual sleep requested"
	if automatic {
		reason = "idle timeout reached"
	}
	c.logger.Info("state_changed", "from", s.State, "to", Stopping, "reason", reason)
	op, cancel := context.WithTimeout(ctx, c.cfg.OperationTimeout)
	defer cancel()
	players, err := c.backend.Players(op)
	if err != nil || players < 0 {
		if err == nil {
			err = errors.New("backend returned a negative player count")
		}
		// No stop has been sent. Only a future successful health poll can
		// reopen the gate; a failed recheck never means empty.
		c.unhealthy(Unknown, "final player check failed; shutdown cancelled", err, true)
		return fmt.Errorf("final player check: %w", err)
	}
	if players != 0 {
		c.update(Ready, "player joined; shutdown cancelled", func(s *Snapshot) {
			s.Players, s.StoppingSince = &players, time.Time{}
			s.LastCheck, s.LastSuccessfulCheck = time.Now(), time.Now()
		})
		return ErrBusy
	}
	c.update(Stopping, "sending graceful stop", func(s *Snapshot) {
		s.LastCheck, s.LastSuccessfulCheck = time.Now(), time.Now()
		s.Players = &players
	})
	if err := c.backend.Stop(op); err != nil {
		// A dropped RCON connection can mean stop succeeded. Remain closed
		// until the process is observed exited; never guess, kill, or retry.
		c.unhealthy(Stopping, "stop result uncertain; waiting for process exit", err, true)
		return fmt.Errorf("graceful stop: %w", err)
	}
	return nil
}

func (c *Controller) unhealthy(state State, reason string, err error, report bool) {
	c.update(state, reason, func(s *Snapshot) {
		s.Players, s.IdleSince = nil, time.Time{}
		s.LastCheck, s.LastError = time.Now(), err.Error()
		if reason == "cannot determine backend state" {
			s.BackendKnown = false
		}
	})
	if report {
		c.reportErrorWithKey(stableErrorKey(err), err.Error())
	}
}

func (c *Controller) update(state State, reason string, mutate func(*Snapshot)) {
	c.mu.Lock()
	before := c.snap.State
	previousPlayers := -1
	if c.snap.Players != nil {
		previousPlayers = *c.snap.Players
	}
	mutate(&c.snap)
	c.snap.State = state
	players := -1
	if c.snap.Players != nil {
		players = *c.snap.Players
	}
	c.mu.Unlock()
	if before != state {
		c.logger.Info("state_changed", "from", before, "to", state, "reason", reason)
	}
	if players >= 0 && previousPlayers != players {
		c.logger.Info("players_changed", "count", players)
	}
}

func (c *Controller) reportError(message string) {
	c.reportErrorWithKey(message, message)
}

// stableErrorKey retains the operation, remote endpoint, wrapping context, and
// underlying cause while omitting the client's ephemeral source address. Each
// health probe uses a new connection; comparing its full net.OpError string
// would therefore turn one persistent outage into a warning on every poll.
// LastError and the first emitted warning still carry the full diagnostic.
func stableErrorKey(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	var op *net.OpError
	if !errors.As(err, &op) {
		return message
	}
	key := op.Op + " " + op.Net
	if op.Addr != nil {
		key += " " + op.Addr.String()
	}
	if cause := stableErrorKey(op.Err); cause != "" {
		key += ": " + cause
	}
	return strings.ReplaceAll(message, op.Error(), key)
}

func (c *Controller) reportErrorWithKey(key, message string) {
	if key == c.reportedError {
		return
	}
	if key == "" {
		if c.reportedError != "" {
			c.logger.Info("backend_health_recovered")
		}
	} else {
		c.logger.Warn("backend_health_error", "error", message)
	}
	c.reportedError = key
}
