package gateway

import (
	"context"
	"net"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/mcproto"
	"github.com/difof/mcs-bootloader/internal/whitelist"
)

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
	s.handleLogin(ctx, conn, cfg, handshake)
}

func (s *Server) handleLogin(ctx context.Context, conn net.Conn, cfg Config, handshake mcproto.Handshake) {
	login, name, err := mcproto.ReadLoginStart(conn, handshake.Protocol)
	if err != nil {
		s.stats.invalidRequests.Add(1)
		s.log.Debug("invalid_login_start", "error", err)
		return
	}
	s.stats.loginRequests.Add(1)
	if s.rejectDraining(conn, handshake.Protocol) {
		return
	}
	if !s.allowPlayer(conn, cfg, handshake.Protocol, name) {
		return
	}
	release, acquired, draining := s.acquireJoin()
	if draining {
		s.rejectDraining(conn, handshake.Protocol)
		return
	}
	if !acquired {
		s.wakeAndDisconnect(ctx, conn, cfg, handshake.Protocol)
		return
	}
	defer release()
	s.routeLogin(ctx, conn, cfg, handshake, login, name)
}

func (s *Server) allowPlayer(conn net.Conn, cfg Config, protocol int, name string) bool {
	allowed, err := whitelist.Check(cfg.ServerDirectory, name)
	if err == nil && allowed {
		return true
	}
	message := cfg.WhitelistMessage
	if err != nil {
		s.stats.whitelistErrors.Add(1)
		message = cfg.FailedMOTD
	} else {
		s.stats.whitelistDenied.Add(1)
	}
	_ = mcproto.WriteDisconnect(conn, protocol, message)
	return false
}

func (s *Server) wakeAndDisconnect(ctx context.Context, conn net.Conn, cfg Config, protocol int) {
	before := s.controller.Snapshot()
	if before.State != control.Ready && s.limiter.allow(clientHost(conn), time.Now()) {
		s.stats.wakeRequests.Add(1)
		wakeCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
		stopCancel := context.AfterFunc(s.admissions, cancel)
		_ = s.controller.Wake(wakeCtx)
		stopCancel()
		cancel()
	} else if before.State != control.Ready {
		s.stats.wakeLimited.Add(1)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if !s.rejectDraining(conn, protocol) {
		_ = mcproto.WriteDisconnect(conn, protocol, disconnectMessage(cfg, s.controller.Snapshot(), time.Now()))
	}
}

func (s *Server) routeLogin(ctx context.Context, conn net.Conn, cfg Config, handshake mcproto.Handshake, login []byte, name string) {
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
	if s.rejectDraining(conn, handshake.Protocol) {
		return
	}
	setKeepAlive(backend)
	stopClose := context.AfterFunc(ctx, func() { backend.Close(); conn.Close() })
	defer stopClose()
	_ = backend.SetWriteDeadline(time.Now().Add(cfg.DialTimeout))
	err = writeLogin(backend, handshake.Raw, login)
	if err != nil {
		s.stats.backendErrors.Add(1)
		s.logBackendError("backend_login_forward_failed", err)
		return
	}
	s.stats.bytesToBackend.Add(uint64(len(handshake.Raw) + len(login)))
	// No gameplay deadlines or parsing: large registries, mod payloads, login
	// extensions, compression, and encryption remain the backend's responsibility.
	_ = backend.SetDeadline(time.Time{})
	_ = conn.SetDeadline(time.Time{})
	s.stats.routed.Add(1)
	s.relaySession(ctx, conn, backend, name, len(handshake.Raw)+len(login))
}

func writeLogin(backend net.Conn, handshake, login []byte) error {
	if err := writeAll(backend, handshake); err != nil {
		return err
	}
	return writeAll(backend, login)
}

func (s *Server) relaySession(ctx context.Context, conn, backend net.Conn, name string, initialBytes int) {
	started := time.Now()
	s.log.Info("connection_opened", "client", clientHost(conn), "name_unverified", name)
	toBackend, toClient, relayErr := relay(ctx, conn, backend, &s.stats)
	args := []any{"client", clientHost(conn), "name_unverified", name, "duration", time.Since(started).Round(time.Millisecond).String(), "bytes_to_backend", toBackend + int64(initialBytes), "bytes_to_client", toClient}
	if relayErr != nil {
		args = append(args, "error", relayErr)
	}
	s.log.Info("connection_closed", args...)
}

func (s *Server) rejectDraining(conn net.Conn, protocol int) bool {
	if !s.isDraining() {
		return false
	}
	s.stats.drainingRejected.Add(1)
	_ = mcproto.WriteDisconnect(conn, protocol, s.cfg.Load().RestartMessage)
	return true
}

func disconnectMessage(cfg Config, snapshot control.Snapshot, now time.Time) string {
	switch snapshot.State {
	case control.Asleep, control.Starting:
		return bootMessage(cfg.BootMessage, snapshot, now)
	case control.Stopping:
		return cfg.StoppingMOTD
	case control.Unknown:
		return cfg.UnknownMOTD
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

func clientHost(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}
