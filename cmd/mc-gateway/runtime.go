package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"mc-gateway/internal/backend"
	"mc-gateway/internal/control"
	"mc-gateway/internal/gateway"
)

// runtimeConfig keeps the applied configuration separate from edits on disk.
// A reload is serialized with wake/sleep, validates everything before commit,
// and never replaces the listener or touches established game connections.
type runtimeConfig struct {
	reloadMu   sync.Mutex
	mu         sync.RWMutex
	cfg        backend.Config
	path       string
	digest     string
	generation uint64
	controller *control.Controller
	gateway    *gateway.Server
	backend    runtimeBackend
	level      *slog.LevelVar
	log        *slog.Logger
}

type runtimeBackend interface {
	Running(context.Context) (bool, error)
	ReconfigureRCON(string, time.Duration, bool)
	Command(context.Context, string, time.Duration) (string, error)
}

type configMetadata struct {
	Generation uint64 `json:"generation"`
	SHA256     string `json:"sha256"`
}
type reloadResult struct {
	configMetadata
	Applied bool     `json:"applied"`
	Changed []string `json:"changed"`
}

func readConfigSnapshot(path string) (backend.Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return backend.Config{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return backend.Config{}, "", err
	}
	if !st.Mode().IsRegular() {
		return backend.Config{}, "", errors.New("configuration must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return backend.Config{}, "", err
	}
	if len(data) > 65536 {
		return backend.Config{}, "", errors.New("configuration exceeds 64 KiB")
	}
	var cfg backend.Config
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		// Allow an older installation to run while the migration is staged.
		cfg = backend.DefaultConfig()
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
			return cfg, "", errors.New("configuration must contain supported fields and value types")
		}
		err = cfg.Validate()
	} else {
		if st.Mode().Perm() != 0600 && st.Mode().Perm() != 0640 {
			return cfg, "", errors.New(".env must have private 0600 or 0640 permissions")
		}
		cfg, err = backend.LoadEnv(data)
	}
	if err != nil {
		return backend.Config{}, "", err
	}
	password, err := cfg.Password()
	if err != nil {
		return backend.Config{}, "", err
	}
	cfg.RCONPassword = password
	digest := sha256.Sum256(data)
	return cfg, hex.EncodeToString(digest[:]), nil
}

func lifecycleConfig(cfg backend.Config) control.Config {
	return control.Config{IdleTimeout: time.Duration(cfg.IdleTimeout), PollInterval: time.Duration(cfg.PollInterval),
		OperationTimeout: time.Duration(cfg.OperationTimeout), StartupTimeout: time.Duration(cfg.StartupTimeout),
		ShutdownWarnAfter: time.Duration(cfg.ShutdownWarnAfter), WakeCooldown: time.Duration(cfg.WakeCooldown)}
}
func trafficConfig(cfg backend.Config) gateway.Config {
	fallback, _ := json.Marshal(map[string]any{
		"version":     map[string]any{"name": cfg.Version, "protocol": cfg.Protocol},
		"players":     map[string]any{"max": cfg.MaxPlayers, "online": 0, "sample": []any{}},
		"description": cfg.SleepingMOTD, "isModded": true,
	})
	return gateway.Config{BackendAddress: cfg.BackendAddress, Protocol: cfg.Protocol, MaxConnections: cfg.MaxConnections,
		HandshakeTimeout: time.Duration(cfg.HandshakeTimeout), DialTimeout: time.Duration(cfg.DialTimeout),
		BootMessage: cfg.BootMessage, SleepingMOTD: cfg.SleepingMOTD, StartingMOTD: cfg.StartingMOTD,
		FailedMOTD: cfg.FailedMOTD, FallbackStatus: fallback}
}
func parseLogLevel(value string) slog.Level {
	if value == "debug" {
		return slog.LevelDebug
	}
	if value == "warn" {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}
func (m *runtimeConfig) snapshot() (backend.Config, configMetadata) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg, configMetadata{Generation: m.generation, SHA256: m.digest}
}

func restartFields(old, next backend.Config) []string {
	fields := []struct {
		name      string
		old, next any
	}{
		{"MC_LISTEN", old.Listen, next.Listen}, {"MC_BACKEND_ADDRESS", old.BackendAddress, next.BackendAddress},
		{"MC_RCON_ADDRESS", old.RCONAddress, next.RCONAddress}, {"MC_BACKEND_SERVICE", old.Service, next.Service},
		{"MC_STATE_DIRECTORY", old.StateDirectory, next.StateDirectory}, {"MC_ADMIN_SOCKET", old.AdminSocket, next.AdminSocket},
		{"MC_PROTOCOL", old.Protocol, next.Protocol}, {"MC_SERVER_DIRECTORY", old.ServerDirectory, next.ServerDirectory},
		{"MC_JAVA_BINARY", old.JavaBinary, next.JavaBinary}, {"MC_JVM_ARGFILE", old.JVMArgFile, next.JVMArgFile},
		{"MC_SERVER_ARGFILE", old.ServerArgFile, next.ServerArgFile},
	}
	var changed []string
	for _, f := range fields {
		if f.old != f.next {
			changed = append(changed, f.name)
		}
	}
	return changed
}

func changedFields(old, next backend.Config) []string {
	before, after := reflect.ValueOf(old), reflect.ValueOf(next)
	var changed []string
	for i := 0; i < before.NumField(); i++ {
		if reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
			continue
		}
		field := before.Type().Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if field.Name == "RCONPassword" {
			name = "rcon_password"
		}
		if name != "" && name != "-" {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	if changed == nil {
		changed = []string{}
	}
	return changed
}

func (m *runtimeConfig) reload(ctx context.Context, expected string) (reloadResult, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	next, digest, err := readConfigSnapshot(m.path)
	if err != nil {
		return reloadResult{}, fmt.Errorf("reload rejected: %w", err)
	}
	if expected != "" && digest != expected {
		return reloadResult{}, errors.New(".env changed after the request was prepared; run task config:apply again")
	}
	old, metadata := m.snapshot()
	if digest == metadata.SHA256 {
		return reloadResult{configMetadata: metadata, Changed: []string{}}, nil
	}
	if fields := restartFields(old, next); len(fields) > 0 {
		return reloadResult{}, fmt.Errorf("reload rejected; these settings require a gateway restart: %s", strings.Join(fields, ", "))
	}
	lifecycle, traffic := lifecycleConfig(next), trafficConfig(next)
	if err := lifecycle.Validate(); err != nil {
		return reloadResult{}, err
	}
	if err := traffic.Validate(); err != nil {
		return reloadResult{}, err
	}
	passwordChanged := old.RCONPassword != next.RCONPassword
	changed := changedFields(old, next)
	err = m.controller.ReconfigureWith(ctx, lifecycle, func() error {
		if passwordChanged {
			checkCtx, cancel := context.WithTimeout(context.Background(), time.Duration(old.OperationTimeout))
			running, err := m.backend.Running(checkCtx)
			cancel()
			if err != nil {
				return errors.New("cannot confirm Minecraft is stopped; RCON password was not changed")
			}
			if running {
				return errors.New("MC_RCON_PASSWORD can change only while Minecraft is stopped; restore its current value, or stop Minecraft before applying")
			}
		}
		// Reconfigure has already been validated and pinned addresses match.
		// There are no fallible commits after this operation succeeds.
		if err := m.gateway.Reconfigure(traffic); err != nil {
			return err
		}
		m.backend.ReconfigureRCON(next.RCONPassword, time.Duration(next.OperationTimeout), passwordChanged)
		m.level.Set(parseLogLevel(next.LogLevel))
		m.mu.Lock()
		m.cfg = next
		m.digest = digest
		m.generation++
		metadata = configMetadata{Generation: m.generation, SHA256: m.digest}
		m.mu.Unlock()
		return nil
	})
	if err != nil {
		return reloadResult{}, err
	}
	m.log.Info("configuration_applied", "generation", metadata.Generation, "changed", changed)
	return reloadResult{configMetadata: metadata, Applied: true, Changed: changed}, nil
}

// Commands use the applied secret and the existing serialized RCON connection.
// Editing .env does not silently alter command authentication before a reload.
func (m *runtimeConfig) command(ctx context.Context, command string, timeout time.Duration) (string, error) {
	// Serialize against reload so the command reply is redacted with the exact
	// credential used for its exchange, including during password rotation.
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
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
		return nil, errors.New(".env changed during validation; run task gateway:restart again")
	}
	old, metadata := m.snapshot()
	if old.RCONPassword != next.RCONPassword {
		return nil, errors.New("restart refused: apply MC_RCON_PASSWORD using task config:apply while Minecraft is stopped before restarting the gateway")
	}
	return map[string]any{"valid": true, "configuration": metadata, "restart_fields": restartFields(old, next)}, nil
}
