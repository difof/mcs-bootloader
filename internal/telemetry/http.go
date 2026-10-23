package telemetry

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/difof/mcs-bootloader/internal/rcon"
)

type endpoint struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
}

func (m *Manager) listen(address string) (*endpoint, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("bind metrics listener: %w", err)
	}
	return &endpoint{
		listener: ln,
		done:     make(chan struct{}),
		server: &http.Server{
			Handler:           http.HandlerFunc(m.serveHTTP),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    8192,
			ErrorLog:          slog.NewLogLogger(m.log.Handler(), slog.LevelDebug),
		},
	}, nil
}

func (ep *endpoint) serve(logger *slog.Logger) {
	go func() {
		defer close(ep.done)
		if err := ep.server.Serve(ep.listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			logger.Error("metrics_listener_failed", "error", err)
		}
	}()
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
