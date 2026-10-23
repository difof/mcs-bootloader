// Package gateway implements a small Minecraft-aware front door and opaque TCP
// relay. It does not interpret, terminate, or translate gameplay packets.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

type Controller interface {
	Wake(context.Context) error
	Snapshot() control.Snapshot
	AcquireJoin() (release func(), ok bool)
	CachedStatus() json.RawMessage
}

type Server struct {
	cfg                       atomic.Pointer[Config]
	controller                Controller
	log                       *slog.Logger
	stats                     counters
	limiter                   wakeLimiter
	mu                        sync.Mutex
	listener                  net.Listener
	connections               map[net.Conn]struct{}
	cancel                    context.CancelFunc
	closed                    bool
	draining                  bool
	activeRelays              int
	relaysChanged             chan struct{}
	admissions                context.Context
	stopAdmissions            context.CancelFunc
	wg                        sync.WaitGroup
	errorMu                   sync.Mutex
	lastBackendWarning        time.Time
	suppressedBackendWarnings uint64
}

func New(cfg Config, controller Controller, logger *slog.Logger) *Server {
	cfg.defaults()
	if logger == nil {
		logger = slog.Default()
	}
	admissions, stopAdmissions := context.WithCancel(context.Background())
	s := &Server{controller: controller, log: logger, connections: make(map[net.Conn]struct{}), relaysChanged: make(chan struct{}), admissions: admissions, stopAdmissions: stopAdmissions}
	cfg.FallbackStatus = append(json.RawMessage(nil), cfg.FallbackStatus...)
	s.cfg.Store(&cfg)
	return s
}

// ConfigSnapshot returns a private copy; callers cannot mutate live fallback
// metadata through the returned JSON byte slice.
func (s *Server) ConfigSnapshot() Config {
	cfg := *s.cfg.Load()
	cfg.FallbackStatus = append(json.RawMessage(nil), cfg.FallbackStatus...)
	return cfg
}

// Reconfigure publishes an immutable configuration for subsequent admissions.
// Existing relays keep their connections, including when the limit is lowered
// below their current count. Address and protocol changes require a restart.
func (s *Server) Reconfigure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.FallbackStatus = append(json.RawMessage(nil), cfg.FallbackStatus...)
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.cfg.Load()
	if cfg.BackendAddress != current.BackendAddress || cfg.Protocol != current.Protocol || cfg.ServerDirectory != current.ServerDirectory {
		return errors.New("backend address, protocol and server directory cannot change at runtime")
	}
	s.cfg.Store(&cfg)
	return nil
}

// Serve owns listener. Cancellation closes gateway connections but deliberately
// leaves backend process management to the independent controller/supervisor.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("nil listener")
	}
	if s.controller == nil {
		listener.Close()
		return errors.New("nil controller")
	}
	if err := s.cfg.Load().Validate(); err != nil {
		listener.Close()
		return fmt.Errorf("invalid gateway configuration: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	if s.closed || s.listener != nil {
		s.mu.Unlock()
		listener.Close()
		return errors.New("gateway can only be served once")
	}
	s.listener, s.cancel = listener, cancel
	s.mu.Unlock()
	stopClose := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stopClose()
	defer func() { _ = s.Close(); s.wg.Wait() }()
	return s.acceptConnections(ctx, listener)
}

func (s *Server) acceptConnections(ctx context.Context, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept client: %w", err)
		}
		if !s.startConnection(ctx, conn) {
			return nil
		}
	}
}

// startConnection publishes the connection and its config snapshot under the
// same lock as reload/close. A capacity rejection does not stop the accept loop.
func (s *Server) startConnection(ctx context.Context, conn net.Conn) bool {
	s.stats.accepted.Add(1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return false
	}
	cfg := *s.cfg.Load()
	if len(s.connections) >= cfg.MaxConnections {
		s.mu.Unlock()
		s.stats.capacityRejected.Add(1)
		conn.Close()
		return true
	}
	s.connections[conn] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer s.closeConnection(conn)
		s.handle(ctx, conn, cfg)
	}()
	return true
}

func (s *Server) closeConnection(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listener, cancel := s.listener, s.cancel
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	s.stopAdmissions()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	return nil
}
