package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/telemetry"
)

// runtimeConfig keeps the applied configuration separate from edits on disk.
// A reload is serialized with wake/sleep, validates everything before commit,
// and never replaces the listener or touches established game connections.
type runtimeConfig struct {
	reloadMu     sync.Mutex
	mu           sync.RWMutex
	cfg          backend.Config
	path         string
	digest       string
	generation   uint64
	controller   *control.Controller
	gateway      *gateway.Server
	backend      runtimeBackend
	level        *slog.LevelVar
	log          *slog.Logger
	metrics      *telemetry.Manager
	shuttingDown atomic.Bool
}

type runtimeBackend interface {
	Running(context.Context) (bool, error)
	ReconfigureRCON(string, time.Duration, bool)
	Command(context.Context, string, time.Duration) (string, error)
}

type configMetadata struct {
	Generation uint64 `json:"generation"`
	SHA256     string `json:"sha256"`
	Source     string `json:"source"`
}
type reloadResult struct {
	configMetadata
	Applied bool     `json:"applied"`
	Changed []string `json:"changed"`
}

func (m *runtimeConfig) snapshot() (backend.Config, configMetadata) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone(), configMetadata{Generation: m.generation, SHA256: m.digest, Source: m.path}
}

// Commands use the applied secret and the existing serialized RCON connection.
// Editing .env does not silently alter command authentication before a reload.
func (m *runtimeConfig) command(ctx context.Context, command string, timeout time.Duration) (string, error) {
	// Serialize against reload so the command reply is redacted with the exact
	// credential used for its exchange, including during password rotation.
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.shuttingDown.Load() {
		return "", errors.New("gateway is shutting down")
	}
	cfg, _ := m.snapshot()
	reply, err := m.backend.Command(ctx, command, timeout)
	if err != nil {
		return "", errors.New(strings.ReplaceAll(operatorError(ctx, err).Error(), cfg.RCONPassword, "[REDACTED]"))
	}
	return strings.ReplaceAll(reply, cfg.RCONPassword, "[REDACTED]"), nil
}

// A restart must not bypass password rotation's serialized stopped-server guard.
// Require a password edit to be applied first; a join between this check and
// restart will then launch Java with the same already-applied credential.
func (m *runtimeConfig) restartCheck(expected string) (map[string]any, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	next, digest, err := readConfigSnapshot(m.path)
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New(".env changed during validation; run mcs-bootloader gateway restart again")
	}
	old, metadata := m.snapshot()
	if old.RCONPassword != next.RCONPassword {
		return nil, errors.New("restart refused: apply MC_RCON_PASSWORD using mcs-bootloader config reload while Minecraft is stopped before restarting the gateway")
	}
	return map[string]any{"valid": true, "configuration": metadata, "restart_fields": restartFields(old, next)}, nil
}
