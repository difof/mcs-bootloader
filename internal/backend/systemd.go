// Package backend connects lifecycle decisions to a separately supervised server.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mc-gateway/internal/mcproto"
	"mc-gateway/internal/rcon"
)

type Systemd struct {
	Service  string
	Address  string
	Protocol int
	RCON     *rcon.Client
	mu       sync.RWMutex
}

func (s *Systemd) Start(ctx context.Context) error {
	// The sudo rule permits only this exact start operation on the named unit.
	// No shell, arbitrary command, or service stop is exposed to the daemon.
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "/usr/bin/systemctl", "start", s.Service)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start backend service: %w: %.2048s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Systemd) Running(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", s.Service, "--property=LoadState", "--property=ActiveState")
	out, err := cmd.Output()
	if err != nil {
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
		return false, fmt.Errorf("backend service is not loaded")
	}
	switch values["ActiveState"] {
	case "active", "activating", "reloading", "refreshing":
		return true, nil
	case "deactivating":
		return true, fmt.Errorf("backend service is stopping")
	case "inactive", "failed":
		return false, nil
	default:
		return false, fmt.Errorf("unknown backend service state %q", values["ActiveState"])
	}
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
		s.RCON = rcon.NewPersistent(old.Address(), password, timeout)
		_ = old.Close()
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
