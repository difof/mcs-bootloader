package telemetry

import (
	"errors"
	"maps"
	"sync"
)

type Prepared struct {
	manager      *Manager
	cfg          Config
	endpoint     *endpoint
	keepEndpoint bool
	once         sync.Once
}

// Prepare binds a replacement listener without serving it. Failure leaves all
// applied settings and the old listener untouched. Always Commit or Abort.
func (m *Manager) Prepare(cfg Config) (*Prepared, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.Directories = maps.Clone(cfg.Directories)
	m.changeMu.Lock()
	m.mu.RLock()
	closed, old, oldCfg := m.closed, m.endpoint, m.cfg
	m.mu.RUnlock()
	if closed {
		m.changeMu.Unlock()
		return nil, errors.New("metrics manager is closed")
	}
	p := &Prepared{manager: m, cfg: cfg, keepEndpoint: cfg.Enabled && old != nil && cfg.Listen == oldCfg.Listen}
	if cfg.Enabled && !p.keepEndpoint {
		var err error
		p.endpoint, err = m.listen(cfg.Listen)
		if err != nil {
			m.changeMu.Unlock()
			return nil, err
		}
	}
	return p, nil
}

func (p *Prepared) Commit() {
	p.once.Do(func() {
		m := p.manager
		old := m.commitConfiguration(p)
		if !p.keepEndpoint && old != nil {
			_ = old.server.Close()
		}
		if p.endpoint != nil {
			p.endpoint.serve(m.log)
		}
		m.changeMu.Unlock()
	})
}

func (p *Prepared) Abort() {
	p.once.Do(func() {
		if p.endpoint != nil {
			_ = p.endpoint.listener.Close()
		}
		p.manager.changeMu.Unlock()
	})
}

func (m *Manager) Reconfigure(cfg Config) error {
	p, err := m.Prepare(cfg)
	if err != nil {
		return err
	}
	p.Commit()
	return nil
}

// commitConfiguration changes the cache and listener pointer under one lock.
// No network or filesystem operation is performed while the lock is held.
func (m *Manager) commitConfiguration(p *Prepared) *endpoint {
	m.mu.Lock()
	old := m.endpoint
	m.cfg = p.cfg
	m.generation++
	if m.cancelPoll != nil {
		m.cancelPoll()
	}
	if m.cancelDirectory != nil {
		m.cancelDirectory()
	}
	nextDirectories := make(map[string]directorySample)
	if p.cfg.Enabled {
		for name, path := range p.cfg.Directories {
			sample := m.directories[name]
			if sample.Path != path {
				sample = directorySample{Path: path}
			}
			sample.InFlight = false
			nextDirectories[name] = sample
		}
	}
	m.directories = nextDirectories
	if !p.keepEndpoint {
		m.endpoint = p.endpoint
	}
	// Applying privacy settings removes already-cached identity labels at
	// once, before the next poll can complete.
	if !p.cfg.PlayerDetails {
		m.cache.removeDetails()
	}
	if !p.cfg.Enabled {
		cleared := newCollection()
		for name, h := range m.cache.Health {
			cleared.Health[name] = health{Errors: h.Errors}
		}
		m.cache = cleared
	}
	// Publish wake notifications with the matching generation. Workers consume
	// pending notifications while reading that snapshot, preventing a late
	// notification from repeating an already-completed initial collection.
	select {
	case m.wake <- struct{}{}:
	default:
	}
	select {
	case m.directoryWake <- struct{}{}:
	default:
	}
	m.mu.Unlock()
	return old
}
