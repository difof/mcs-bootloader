package control

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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
		c.observeStopped(s)
		return
	}
	s = c.observeRunning(s)
	if s.State == Stopping {
		c.warnSlowShutdown(s)
		return
	}
	players, err := c.checkReadiness(op)
	if err != nil {
		c.notReady(err)
		return
	}
	if !c.wasReady && c.bootLearning && !c.verifyBootProcess(op) {
		return
	}
	c.markReady(players)
	s = c.Snapshot()
	if !s.IdleSince.IsZero() && c.now().Sub(s.IdleSince) >= c.cfg.IdleTimeout {
		_ = c.sleep(ctx, true)
	}
}

func (c *Controller) observeStopped(previous Snapshot) {
	c.wasReady, c.bootLearning = false, false
	c.processIdentity = ""
	state, reason := Asleep, "backend is stopped"
	message := ""
	if previous.State == Failed {
		state, message = Failed, previous.LastError
	} else if previous.State == Ready || previous.State == Starting || (previous.BackendRunning && previous.State != Stopping) {
		state, reason, message = Failed, "backend exited unexpectedly", "backend exited unexpectedly"
	}
	c.update(state, reason, func(s *Snapshot) {
		s.BackendRunning, s.BackendKnown = false, true
		s.BootStartedAt = time.Time{}
		s.Players, s.IdleSince = nil, time.Time{}
		s.LastCheck, s.LastError = c.now(), message
		if state == Failed && reason == "backend exited unexpectedly" {
			s.NextWakeAt = c.now().Add(c.cfg.WakeCooldown)
		}
		if state == Asleep {
			s.StoppingSince = time.Time{}
		}
	})
	c.reportError(message)
}

func (c *Controller) observeRunning(previous Snapshot) Snapshot {
	if c.observeProcess() {
		c.wasReady = false
		c.update(Starting, "backend process changed", func(s *Snapshot) {
			s.StartedAt, s.BootStartedAt = c.now(), time.Time{}
			s.Players, s.IdleSince, s.StoppingSince = nil, time.Time{}, time.Time{}
		})
		previous = c.Snapshot()
	}
	c.mu.Lock()
	c.snap.BackendRunning, c.snap.BackendKnown = true, true
	if c.snap.StartedAt.IsZero() || previous.State == Asleep || (!previous.BackendRunning && !c.bootLearning) {
		c.snap.StartedAt = c.now()
		c.snap.BootStartedAt, c.snap.ReadySince = time.Time{}, time.Time{}
		c.wasReady = false
	}
	c.mu.Unlock()
	return previous
}

func (c *Controller) warnSlowShutdown(s Snapshot) {
	if s.StoppingSince.IsZero() || c.now().Sub(s.StoppingSince) < c.cfg.ShutdownWarnAfter {
		return
	}
	msg := "graceful shutdown is taking longer than expected; waiting without sending signals"
	c.update(Stopping, "waiting for graceful shutdown", func(s *Snapshot) { s.LastError = msg })
	c.reportError(msg)
}

func (c *Controller) checkReadiness(ctx context.Context) (int, error) {
	players, err := c.backend.Players(ctx)
	if err == nil && players < 0 {
		err = errors.New("backend returned a negative player count")
	}
	if err != nil {
		return 0, err
	}
	raw, err := c.backend.Status(ctx)
	if err == nil {
		err = c.SetCachedStatus(raw)
	}
	if err != nil {
		return 0, fmt.Errorf("server status: %w", err)
	}
	return players, nil
}

func (c *Controller) markReady(players int) {
	now := c.now()
	if !c.wasReady {
		c.recordSuccessfulBoot(now)
	}
	c.wasReady = true
	c.update(Ready, "backend ready", func(s *Snapshot) {
		s.Players = &players
		s.LastCheck, s.LastSuccessfulCheck, s.LastError = now, now, ""
		if s.ReadySince.IsZero() {
			s.ReadySince = now
		}
		if players > 0 || s.ActiveConnections > 0 {
			s.IdleSince = time.Time{}
		} else if s.IdleSince.IsZero() {
			s.IdleSince = now
		}
	})
	c.reportError("")
}

func (c *Controller) notReady(err error) {
	state, report := Starting, false
	s := c.Snapshot()
	if c.wasReady {
		state, report = Unknown, true
	} else if !s.StartedAt.IsZero() && c.now().Sub(s.StartedAt) >= c.cfg.StartupTimeout {
		state, report = Failed, true
		err = fmt.Errorf("startup readiness timeout; backend left running: %w", err)
	}
	c.unhealthy(state, "backend not ready", err, report)
}
