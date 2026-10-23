// Package gateway implements a small Minecraft-aware front door and opaque TCP
// relay. It does not interpret, terminate, or translate gameplay packets.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/mcproto"
)

type Controller interface {
	Wake(context.Context) error
	Snapshot() control.Snapshot
	AcquireJoin() (release func(), ok bool)
	CachedStatus() json.RawMessage
}

type Config struct {
	BackendAddress   string
	Protocol         int
	MaxConnections   int
	HandshakeTimeout time.Duration
	DialTimeout      time.Duration
	BootMessage      string
	SleepingMOTD     string
	StartingMOTD     string
	FailedMOTD       string
	FallbackStatus   json.RawMessage
}

// Validate checks a complete configuration without applying defaults. The
// startup-only backend address and protocol are additionally pinned by Server.
func (cfg Config) Validate() error {
	host, port, err := net.SplitHostPort(cfg.BackendAddress)
	if err != nil || host == "" {
		return errors.New("backend address must contain a host and port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("backend port must be between 1 and 65535")
	}
	if cfg.Protocol <= 0 {
		return errors.New("protocol must be positive")
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > 4096 {
		return errors.New("maximum connections must be between 1 and 4096")
	}
	if cfg.HandshakeTimeout <= 0 || cfg.DialTimeout <= 0 {
		return errors.New("handshake and dial timeouts must be positive")
	}
	for _, message := range []string{cfg.BootMessage, cfg.SleepingMOTD, cfg.StartingMOTD, cfg.FailedMOTD} {
		if len(message) == 0 || len(message) > 4096 {
			return errors.New("messages must contain 1-4096 bytes")
		}
	}
	if len(cfg.FallbackStatus) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cfg.FallbackStatus, &obj); err != nil || obj == nil {
			return errors.New("fallback status must be a JSON object")
		}
	}
	return nil
}

// Stats are aggregate counters, intentionally replacing noisy per-ping and
// malformed-request log messages. They never include client payloads.
type Stats struct {
	Accepted         uint64 `json:"accepted"`
	CapacityRejected uint64 `json:"capacity_rejected"`
	InvalidRequests  uint64 `json:"invalid_requests"`
	StatusRequests   uint64 `json:"status_requests"`
	LoginRequests    uint64 `json:"login_requests"`
	WakeRequests     uint64 `json:"wake_requests"`
	WakeLimited      uint64 `json:"wake_limited"`
	Routed           uint64 `json:"routed"`
	BackendErrors    uint64 `json:"backend_errors"`
}

type counters struct {
	accepted, capacityRejected, invalidRequests, statusRequests     atomic.Uint64
	loginRequests, wakeRequests, wakeLimited, routed, backendErrors atomic.Uint64
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
	wg                        sync.WaitGroup
	errorMu                   sync.Mutex
	lastBackendWarning        time.Time
	suppressedBackendWarnings uint64
}

func New(cfg Config, controller Controller, logger *slog.Logger) *Server {
	if cfg.Protocol == 0 {
		cfg.Protocol = 767
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 64
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 8 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	if cfg.BootMessage == "" {
		cfg.BootMessage = "Server is booting up. Please reconnect shortly."
	}
	if cfg.SleepingMOTD == "" {
		cfg.SleepingMOTD = "Sleeping - join to start the server"
	}
	if cfg.StartingMOTD == "" {
		cfg.StartingMOTD = "Server is starting. Please reconnect shortly."
	}
	if cfg.FailedMOTD == "" {
		cfg.FailedMOTD = "Server is temporarily unavailable. Please try again shortly."
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{controller: controller, log: logger, connections: make(map[net.Conn]struct{})}
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
	if cfg.BackendAddress != current.BackendAddress || cfg.Protocol != current.Protocol {
		return errors.New("backend address and protocol cannot change at runtime")
	}
	s.cfg.Store(&cfg)
	return nil
}

func (s *Server) Stats() Stats {
	return Stats{
		Accepted: s.stats.accepted.Load(), CapacityRejected: s.stats.capacityRejected.Load(),
		InvalidRequests: s.stats.invalidRequests.Load(), StatusRequests: s.stats.statusRequests.Load(),
		LoginRequests: s.stats.loginRequests.Load(), WakeRequests: s.stats.wakeRequests.Load(),
		WakeLimited: s.stats.wakeLimited.Load(), Routed: s.stats.routed.Load(), BackendErrors: s.stats.backendErrors.Load(),
	}
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
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept client: %w", err)
		}
		s.stats.accepted.Add(1)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		// The same lock serializes limit publication with new admissions.
		// Each connection keeps one config snapshot through its handshake.
		cfg := *s.cfg.Load()
		if len(s.connections) >= cfg.MaxConnections {
			s.mu.Unlock()
			s.stats.capacityRejected.Add(1)
			conn.Close()
			continue
		}
		s.connections[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() { conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock() }()
			s.handle(ctx, conn, cfg)
		}()
	}
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

func (s *Server) handle(ctx context.Context, conn net.Conn, cfg Config) {
	setKeepAlive(conn)
	_ = conn.SetDeadline(time.Now().Add(cfg.HandshakeTimeout))
	handshake, err := mcproto.ReadHandshake(conn)
	if err != nil {
		s.stats.invalidRequests.Add(1)
		s.log.Debug("invalid_handshake", "error", err)
		return
	}
	if handshake.NextState == 1 {
		s.stats.statusRequests.Add(1)
		// The status handler has a bounded deadline, including its optional ping.
		_ = mcproto.HandleStatus(conn, s.statusWithConfig(cfg))
		return
	}
	if handshake.NextState != 2 && handshake.NextState != 3 {
		s.stats.invalidRequests.Add(1)
		return
	}
	login, name, err := mcproto.ReadLoginStart(conn, handshake.Protocol)
	if err != nil {
		s.stats.invalidRequests.Add(1)
		s.log.Debug("invalid_login_start", "error", err)
		return
	}
	s.stats.loginRequests.Add(1)
	release, acquired := s.controller.AcquireJoin()
	if !acquired {
		before := s.controller.Snapshot()
		if before.State != control.Ready && s.limiter.allow(clientHost(conn), time.Now()) {
			s.stats.wakeRequests.Add(1)
			wakeCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
			_ = s.controller.Wake(wakeCtx)
			cancel()
		} else if before.State != control.Ready {
			s.stats.wakeLimited.Add(1)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = mcproto.WriteDisconnect(conn, handshake.Protocol, disconnectMessage(cfg, s.controller.Snapshot().State))
		return
	}
	defer release()
	dialer := net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	backend, err := dialer.DialContext(ctx, "tcp", cfg.BackendAddress)
	if err != nil {
		s.stats.backendErrors.Add(1)
		s.logBackendError("backend_connect_failed", err)
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = mcproto.WriteDisconnect(conn, handshake.Protocol, cfg.FailedMOTD)
		return
	}
	defer backend.Close()
	setKeepAlive(backend)
	stopClose := context.AfterFunc(ctx, func() { backend.Close(); conn.Close() })
	defer stopClose()
	_ = backend.SetWriteDeadline(time.Now().Add(cfg.DialTimeout))
	if err = writeAll(backend, handshake.Raw); err == nil {
		err = writeAll(backend, login)
	}
	if err != nil {
		s.stats.backendErrors.Add(1)
		s.logBackendError("backend_login_forward_failed", err)
		return
	}
	// No gameplay deadlines or parsing: large registries, mod payloads, login
	// extensions, compression, and encryption remain the backend's responsibility.
	_ = backend.SetDeadline(time.Time{})
	_ = conn.SetDeadline(time.Time{})
	s.stats.routed.Add(1)
	started := time.Now()
	s.log.Info("connection_opened", "client", clientHost(conn), "name_unverified", name)
	toBackend, toClient, relayErr := relay(ctx, conn, backend)
	args := []any{"client", clientHost(conn), "name_unverified", name, "duration", time.Since(started).Round(time.Millisecond).String(), "bytes_to_backend", toBackend + int64(len(handshake.Raw)+len(login)), "bytes_to_client", toClient}
	if relayErr != nil {
		args = append(args, "error", relayErr)
	}
	s.log.Info("connection_closed", args...)
}

func disconnectMessage(cfg Config, state control.State) string {
	switch state {
	case control.Asleep, control.Starting:
		return cfg.BootMessage
	case control.Stopping:
		return "Server is saving and shutting down. Please reconnect shortly."
	case control.Unknown:
		return "Server health is being checked. Please reconnect shortly."
	default:
		return cfg.FailedMOTD
	}
}

// A stopped/unhealthy backend can attract many repeated join attempts before
// the next controller poll. Preserve counts without flooding the journal.
func (s *Server) logBackendError(event string, err error) {
	now := time.Now()
	s.errorMu.Lock()
	if !s.lastBackendWarning.IsZero() && now.Sub(s.lastBackendWarning) < time.Minute {
		s.suppressedBackendWarnings++
		s.errorMu.Unlock()
		return
	}
	suppressed := s.suppressedBackendWarnings
	s.suppressedBackendWarnings = 0
	s.lastBackendWarning = now
	s.errorMu.Unlock()
	s.log.Warn(event, "error", err, "suppressed_since_last_warning", suppressed)
}

func setKeepAlive(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
}

func clientHost(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type relayResult struct {
	direction int
	bytes     int64
	err       error
}

func relay(ctx context.Context, client, backend net.Conn) (toBackend, toClient int64, err error) {
	results := make(chan relayResult, 2)
	copyDirection := func(direction int, dst, src net.Conn) {
		n, copyErr := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		results <- relayResult{direction: direction, bytes: n, err: copyErr}
	}
	go copyDirection(0, backend, client)
	go copyDirection(1, client, backend)
	first := <-results
	if first.err != nil {
		client.Close()
		backend.Close()
	}
	// After one peer has ended its stream, allow the other direction to drain.
	// This timeout applies only to a closing session, never to live gameplay.
	drain := time.NewTimer(5 * time.Second)
	defer drain.Stop()
	var second relayResult
	select {
	case second = <-results:
	case <-ctx.Done():
		client.Close()
		backend.Close()
		second = <-results
	case <-drain.C:
		client.Close()
		backend.Close()
		second = <-results
	}
	for _, r := range []relayResult{first, second} {
		if r.direction == 0 {
			toBackend = r.bytes
		} else {
			toClient = r.bytes
		}
		if r.err != nil && !errors.Is(r.err, net.ErrClosed) && ctx.Err() == nil {
			err = r.err
		}
	}
	return
}
