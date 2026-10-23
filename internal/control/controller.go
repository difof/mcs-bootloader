// Package control coordinates a separately supervised Minecraft server. It
// never signals or kills that server: the only shutdown operation is RCON stop.
package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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
	wasReady        bool
	reportedError   string
	now             func() time.Time
	bootLearning    bool
	processIdentity string
}

func New(cfg Config, backend Backend, logger *slog.Logger) *Controller {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		cfg: cfg, backend: backend, logger: logger, now: time.Now,
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
			c.handleRequest(ctx, r, ticker)
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}
