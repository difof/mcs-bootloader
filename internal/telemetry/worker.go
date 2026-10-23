package telemetry

import (
	"context"
	"time"
)

func (m *Manager) run() {
	defer close(m.done)
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.mu.RLock()
		select {
		case <-m.wake:
		default:
		}
		cfg, generation, old := m.cfg, m.generation, m.cache.clone()
		m.mu.RUnlock()
		if cfg.Enabled {
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
			m.cancelPoll = cancel
			m.mu.Unlock()
			if !m.clockLoaded {
				m.clockTicks, m.bootTime = processClock(ctx)
				m.clockLoaded = ctx.Err() == nil
			}
			next := m.collect(ctx, cfg, old)
			m.mu.Lock()
			if generation == m.generation && !m.closed && ctx.Err() == nil {
				m.cache = next
			}
			m.cancelPoll = nil
			m.mu.Unlock()
			cancel()
		}
		wait := cfg.PollInterval
		if wait <= 0 {
			wait = time.Hour
		}
		timer := time.NewTimer(wait)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
