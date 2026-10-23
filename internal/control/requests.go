package control

import (
	"context"
	"sync/atomic"
	"time"
)

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

func (c *Controller) handleRequest(ctx context.Context, r request, ticker *time.Ticker) {
	if r.configuration != nil {
		r.reply <- c.applyConfiguration(r, ticker)
		return
	}
	if err := r.ctx.Err(); err != nil {
		r.reply <- err
		return
	}
	// Once accepted, the operation belongs to the controller's lifetime,
	// not the connecting client's. Disconnects cannot cancel a boot.
	if r.sleep {
		r.reply <- c.sleep(ctx, false)
	} else {
		r.reply <- c.wake(ctx)
	}
}

func (c *Controller) applyConfiguration(r request, ticker *time.Ticker) error {
	update := r.configuration
	if err := r.ctx.Err(); err != nil {
		update.phase.CompareAndSwap(0, 2)
		return err
	}
	if !update.phase.CompareAndSwap(0, 1) {
		return r.ctx.Err()
	}
	if update.beforeCommit != nil {
		if err := update.beforeCommit(); err != nil {
			return err
		}
	}
	// A configuration commit has no external I/O. Preserve state,
	// leases, and the existing idle timestamp across the change.
	pollChanged := c.cfg.PollInterval != update.cfg.PollInterval
	c.cfg = update.cfg
	if pollChanged {
		ticker.Reset(c.cfg.PollInterval)
	}
	return nil
}
