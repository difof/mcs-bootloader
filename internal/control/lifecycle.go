package control

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotRunning     = errors.New("lifecycle controller is not running")
	ErrStopping       = errors.New("backend is stopping; retry after it exits")
	ErrBusy           = errors.New("players or connections are active")
	ErrUnknownPlayers = errors.New("player count is unknown; refusing shutdown")
	ErrCooldown       = errors.New("wake cooldown is active")
	ErrQueueFull      = errors.New("lifecycle request queue is full")
)

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
		changed := c.observeProcess()
		if s.State == Asleep || changed {
			c.wasReady = false
			c.bootLearning = false
			c.update(Starting, "adopting running backend", func(s *Snapshot) {
				s.BackendRunning, s.BackendKnown = true, true
				s.StartedAt = c.now()
				s.BootStartedAt, s.ReadySince = time.Time{}, time.Time{}
				s.Players, s.IdleSince = nil, time.Time{}
			})
		}
		return nil
	}
	if c.now().Before(s.NextWakeAt) {
		return ErrCooldown
	}
	c.wasReady = false
	c.bootLearning = true
	c.processIdentity = ""
	now := c.now()
	c.update(Starting, "wake requested", func(s *Snapshot) {
		s.BackendRunning, s.BackendKnown = false, true
		s.Players, s.IdleSince = nil, time.Time{}
		s.StartedAt, s.NextWakeAt = now, now.Add(c.cfg.WakeCooldown)
		s.BootStartedAt, s.ReadySince = now, time.Time{}
		s.StoppingSince, s.LastError = time.Time{}, ""
	})
	if err := c.backend.Start(op); err != nil {
		c.bootLearning = false
		c.unhealthy(Failed, "backend start failed", err, true)
		return fmt.Errorf("start backend: %w", err)
	}
	// A concrete backend can capture its process identity while starting. If
	// it cannot, skip learning rather than timing a later unrelated process.
	if identified, ok := c.backend.(processIdentifiedBackend); ok {
		c.processIdentity = identified.ProcessIdentity()
		if c.processIdentity == "" {
			c.bootLearning = false
		}
	}
	return nil
}

func (c *Controller) sleep(ctx context.Context, automatic bool) error {
	previous, err := c.reserveSleep(automatic)
	if err != nil || previous == Asleep {
		return err
	}
	reason := "manual sleep requested"
	if automatic {
		reason = "idle timeout reached"
	}
	c.logger.Info("state_changed", "from", previous, "to", Stopping, "reason", reason)
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
			s.LastCheck, s.LastSuccessfulCheck = c.now(), c.now()
			s.ReadySince = c.now()
		})
		return ErrBusy
	}
	c.update(Stopping, "sending graceful stop", func(s *Snapshot) {
		s.LastCheck, s.LastSuccessfulCheck = c.now(), c.now()
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

// reserveSleep excludes new join leases before the final RCON check. The
// reservation and its eligibility checks share the snapshot mutex.
func (c *Controller) reserveSleep(automatic bool) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.snap
	if s.State == Asleep {
		return s.State, nil
	}
	if s.State == Stopping {
		return s.State, ErrStopping
	}
	if s.ActiveConnections != 0 || (s.Players != nil && *s.Players > 0) {
		return s.State, ErrBusy
	}
	if s.State != Ready || s.Players == nil {
		return s.State, ErrUnknownPlayers
	}
	if automatic && (s.IdleSince.IsZero() || c.now().Sub(s.IdleSince) < c.cfg.IdleTimeout) {
		return s.State, ErrBusy
	}
	c.snap.State, c.snap.StoppingSince = Stopping, c.now()
	c.snap.IdleSince = time.Time{}
	c.snap.ReadySince = time.Time{}
	return s.State, nil
}
