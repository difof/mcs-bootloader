// Package backend connects lifecycle decisions to a separately supervised server.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/difof/mcs-bootloader/internal/mcproto"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

type Systemd struct {
	Service    string
	Address    string
	Protocol   int
	RCON       *rcon.Client
	mu         sync.RWMutex
	mainPID    atomic.Int64
	invocation atomic.Pointer[string]
}

func (s *Systemd) Start(ctx context.Context) error {
	// The sudo rule permits only this exact start operation on the named unit.
	// No shell, arbitrary command, or service stop is exposed to the daemon.
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "/usr/bin/systemctl", "start", s.Service)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start backend service: %w: %.2048s", err, strings.TrimSpace(string(out)))
	}
	// Capture the service invocation before returning to the controller so a
	// later independent restart cannot be learned as this startup. Failure to
	// inspect does not undo a successful start; it merely disables that sample.
	_, _ = s.Running(ctx)
	return nil
}

func (s *Systemd) Running(ctx context.Context) (bool, error) {
	s.invocation.Store(nil)
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", s.Service, "--property=LoadState", "--property=ActiveState", "--property=MainPID", "--property=InvocationID")
	out, err := cmd.Output()
	if err != nil {
		s.mainPID.Store(0)
		return false, fmt.Errorf("inspect backend service: %w", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		s.mainPID.Store(0)
		return false, fmt.Errorf("backend service is not loaded")
	}
	pid, _ := strconv.ParseInt(values["MainPID"], 10, 32)
	if pid < 0 {
		pid = 0
	}
	s.mainPID.Store(pid)
	switch values["ActiveState"] {
	case "active", "activating", "reloading", "refreshing":
		if id := values["InvocationID"]; validInvocationID(id) {
			s.invocation.Store(&id)
		}
		return true, nil
	case "deactivating":
		return true, fmt.Errorf("backend service is stopping")
	case "inactive", "failed":
		s.mainPID.Store(0)
		return false, nil
	default:
		s.mainPID.Store(0)
		return false, fmt.Errorf("unknown backend service state %q", values["ActiveState"])
	}
}

// MainPID is the last lifecycle inspection's PID; it never starts a process or
// queries systemd during a metrics scrape. Collectors verify /proc start time.
func (s *Systemd) MainPID() int { return int(s.mainPID.Load()) }

// ProcessIdentity is systemd's per-service-start identifier, cached by Running.
// It has no wall-clock or PID-reuse ambiguity and performs no I/O.
func (s *Systemd) ProcessIdentity() string {
	if id := s.invocation.Load(); id != nil {
		return *id
	}
	return ""
}

func validInvocationID(value string) bool {
	if len(value) != 32 || value == strings.Repeat("0", 32) {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (s *Systemd) RCONStats() rcon.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.RCON.Stats()
}

func (s *Systemd) Players(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.RCON.Players(ctx)
}
func (s *Systemd) Stop(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.RCON.Stop(ctx)
}
func (s *Systemd) Command(ctx context.Context, command string, timeout time.Duration) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.RCON.CommandWithTimeout(ctx, command, timeout)
}

// ReconfigureRCON is called by the serialized lifecycle worker. A password
// change must first establish that the backend is stopped. Active commands
// finish before replacement, and healthy connections survive timeout changes.
func (s *Systemd) ReconfigureRCON(password string, timeout time.Duration, replace bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if replace {
		old := s.RCON
		_ = old.Close()
		s.RCON = old.Replacement(password, timeout)
	} else {
		s.RCON.SetTimeout(timeout)
	}
}

func (s *Systemd) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.RCON.Close()
}
func (s *Systemd) Status(ctx context.Context) (json.RawMessage, error) {
	return mcproto.QueryStatus(ctx, s.Address, s.Protocol)
}
