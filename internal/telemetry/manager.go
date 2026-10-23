// Package telemetry exports bounded, read-only Prometheus telemetry. Background
// polling owns all RCON and filesystem work; HTTP scrapes never contact Minecraft.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/difof/mcs-bootloader/internal/backend"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

type Config struct {
	Enabled                                        bool
	Listen                                         string
	PollInterval, OperationTimeout, PlayerInterval time.Duration
	PlayerDetails                                  bool
	ServerDirectory                                string
	Directories                                    map[string]string
	DirectoryInterval, DirectoryTimeout            time.Duration
}

type Snapshot struct {
	Lifecycle        control.Snapshot
	Traffic          gateway.Stats
	IdleTimeout      time.Duration
	ConfigGeneration uint64
}

type Source struct {
	Snapshot  func() Snapshot
	Command   func(context.Context, string, time.Duration) (string, error)
	PID       func() int
	RCONStats func() rcon.Stats
	Status    func() json.RawMessage
}

type endpoint struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
}

type Manager struct {
	source                       Source
	version                      string
	log                          *slog.Logger
	changeMu                     sync.Mutex // held by Prepare until Commit/Abort
	mu                           sync.RWMutex
	cfg                          Config
	generation                   uint64
	endpoint                     *endpoint
	closed                       bool
	cancelPoll                   context.CancelFunc
	cancelDirectory              context.CancelFunc
	directories                  map[string]directorySample
	directoryWake, directoryDone chan struct{}
	directoryScan                func(context.Context, string) (directoryTotals, error)
	cache                        collection
	wake                         chan struct{}
	ctx                          context.Context
	cancel                       context.CancelFunc
	done                         chan struct{}
	clockTicks                   float64
	clockLoaded                  bool
	bootTime                     float64
	requests, requestErrors      atomic.Uint64
	requestNanos                 atomic.Uint64
}

// New is inert until a prepared configuration is committed. It does not open a
// listener, query Minecraft, attach to Java, or start the game server.
func New(source Source, version string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{source: source, version: version, log: logger, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), cache: newCollection(), directories: make(map[string]directorySample), directoryWake: make(chan struct{}, 1), directoryDone: make(chan struct{}), directoryScan: scanDirectory}
	go m.run()
	go m.runDirectories()
	return m
}

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
	if cfg.DirectoryInterval <= 0 || cfg.DirectoryTimeout <= 0 {
		return nil, errors.New("directory metrics intervals must be positive")
	}
	if err := backend.ValidateMetricsDirectories(cfg.Directories); err != nil {
		return nil, err
	}
	cfg.Directories = maps.Clone(cfg.Directories)
	if cfg.PollInterval <= 0 || cfg.OperationTimeout <= 0 || cfg.PlayerInterval <= 0 {
		return nil, errors.New("metrics intervals must be positive")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, errors.New("metrics listener must be a numeric IP and port")
	}
	if _, err = netip.ParseAddr(host); err != nil {
		return nil, errors.New("metrics listener must use a numeric IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("metrics listener port must be between 1 and 65535")
	}
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
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			m.changeMu.Unlock()
			return nil, fmt.Errorf("bind metrics listener: %w", err)
		}
		p.endpoint = &endpoint{listener: ln, done: make(chan struct{})}
		p.endpoint.server = &http.Server{Handler: http.HandlerFunc(m.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: slog.NewLogLogger(m.log.Handler(), slog.LevelDebug)}
	}
	return p, nil
}

func (p *Prepared) Commit() {
	p.once.Do(func() {
		m := p.manager
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
		m.mu.Unlock()
		if !p.keepEndpoint && old != nil {
			_ = old.server.Close()
		}
		if p.endpoint != nil {
			ep := p.endpoint
			go func() {
				defer close(ep.done)
				if err := ep.server.Serve(ep.listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
					m.log.Error("metrics_listener_failed", "error", err)
				}
			}()
		}
		select {
		case m.wake <- struct{}{}:
		default:
		}
		select {
		case m.directoryWake <- struct{}{}:
		default:
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

func (m *Manager) Close() error {
	m.changeMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.changeMu.Unlock()
		<-m.done
		return nil
	}
	m.closed = true
	m.cancel()
	if m.cancelPoll != nil {
		m.cancelPoll()
	}
	if m.cancelDirectory != nil {
		m.cancelDirectory()
	}
	ep := m.endpoint
	m.endpoint = nil
	m.mu.Unlock()
	m.changeMu.Unlock()
	if ep != nil {
		_ = ep.server.Close()
	}
	<-m.done
	if ep != nil {
		<-ep.done
	}
	return nil
}

func (m *Manager) run() {
	defer close(m.done)
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.mu.RLock()
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

func (m *Manager) serveHTTP(w http.ResponseWriter, req *http.Request) {
	started := time.Now()
	defer func() { m.requests.Add(1); m.requestNanos.Add(uint64(time.Since(started))) }()
	if req.URL.Path != "/metrics" {
		m.requestErrors.Add(1)
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		m.requestErrors.Add(1)
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m.mu.RLock()
	cfg, cache := m.cfg, m.cache.clone()
	m.mu.RUnlock()
	if !cfg.Enabled {
		http.NotFound(w, req)
		return
	}
	var snap Snapshot
	if m.source.Snapshot != nil {
		snap = m.source.Snapshot()
	}
	var stats rcon.Stats
	if m.source.RCONStats != nil {
		stats = m.source.RCONStats()
	}
	points := m.points(cfg, cache, snap, stats)
	data := encode(points)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if req.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// Handler also serves the private admin socket's read-only metrics operation.
// It performs only cached/snapshot reads, just like the TCP metrics listener.
func (m *Manager) Handler() http.Handler { return http.HandlerFunc(m.serveHTTP) }
