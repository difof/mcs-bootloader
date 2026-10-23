package telemetry

import (
	"context"
	"sort"
	"time"
)

type directorySample struct {
	Path                     string
	Totals                   directoryTotals
	Success, InFlight        bool
	LastAttempt, LastSuccess time.Time
	Duration                 time.Duration
	Errors                   uint64
}

// runDirectories is separate from the regular metrics worker: a backup scan
// cannot delay player/JVM samples, lifecycle polling, socket requests or HTTP.
// There is at most one scan in flight; reload cancels it, never starts another
// concurrent traversal. OS filesystem calls can outlive cancellation on a hung
// mount, so Close does not wait for this read-only worker before exiting.
func (m *Manager) runDirectories() {
	defer close(m.directoryDone)
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.mu.RLock()
		select {
		case <-m.directoryWake:
		default:
		}
		cfg, generation := m.cfg, m.generation
		scan := m.directoryScan
		m.mu.RUnlock()
		if cfg.Enabled && len(cfg.Directories) > 0 {
			ctx, cancel := context.WithCancel(m.ctx)
			m.mu.Lock()
			if m.closed || m.ctx.Err() != nil {
				m.mu.Unlock()
				cancel()
				return
			}
			if generation != m.generation {
				m.mu.Unlock()
				cancel()
				continue
			}
			m.cancelDirectory = cancel
			m.mu.Unlock()
			m.scanConfiguredDirectories(ctx, cfg, generation, scan)
			m.mu.Lock()
			m.cancelDirectory = nil
			m.mu.Unlock()
			cancel()
		}
		interval := cfg.DirectoryInterval
		if interval <= 0 {
			interval = time.Hour
		}
		timer := time.NewTimer(interval)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-m.directoryWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Manager) scanConfiguredDirectories(ctx context.Context, cfg Config, generation uint64, scan func(context.Context, string) (directoryTotals, error)) {
	names := make([]string, 0, len(cfg.Directories))
	for name := range cfg.Directories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !m.scanConfiguredDirectory(ctx, cfg, generation, name, scan) {
			return
		}
	}
}

func (m *Manager) scanConfiguredDirectory(ctx context.Context, cfg Config, generation uint64, name string, scan func(context.Context, string) (directoryTotals, error)) bool {
	if ctx.Err() != nil {
		return false
	}
	path := cfg.Directories[name]
	started := time.Now()
	m.mu.Lock()
	if generation != m.generation || m.closed {
		m.mu.Unlock()
		return false
	}
	sample := m.directories[name]
	sample.Path = path
	sample.InFlight = true
	sample.LastAttempt = started
	m.directories[name] = sample
	m.mu.Unlock()
	budget, stop := context.WithTimeout(ctx, cfg.DirectoryTimeout)
	totals, err := scan(budget, path)
	if err == nil {
		err = budget.Err()
	}
	stop()
	if ctx.Err() != nil {
		return false
	}
	sample.InFlight = false
	sample.Duration = time.Since(started)
	sample.Success = err == nil
	if err == nil {
		sample.Totals = totals
		sample.LastSuccess = time.Now()
	} else {
		sample.Errors++
	}
	m.mu.Lock()
	if generation == m.generation && !m.closed {
		m.directories[name] = sample
	}
	m.mu.Unlock()
	return true
}
