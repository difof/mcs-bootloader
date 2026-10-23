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
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func readConfigSnapshot(path string) (backend.Config, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	return gateway.Config{BackendAddress: cfg.BackendAddress, ServerDirectory: cfg.ServerDirectory, Protocol: cfg.Protocol, MaxConnections: cfg.MaxConnections,
		HandshakeTimeout: time.Duration(cfg.HandshakeTimeout), DialTimeout: time.Duration(cfg.DialTimeout),
		BootMessage: cfg.BootMessage, SleepingMOTD: cfg.SleepingMOTD, StartingMOTD: cfg.StartingMOTD,
		StoppingMOTD: cfg.StoppingMOTD, UnknownMOTD: cfg.UnknownMOTD, FailedMOTD: cfg.FailedMOTD,
		OverrideMOTD: cfg.OverrideMOTD, MOTD: cfg.MOTD, RestartMessage: cfg.RestartMessage,
		WhitelistMessage: cfg.WhitelistMessage, FallbackStatus: fallback}
}
func metricsConfig(cfg backend.Config) telemetry.Config {
	return telemetry.Config{Enabled: cfg.MetricsEnabled, Listen: cfg.MetricsListen,
		PollInterval: time.Duration(cfg.MetricsPollInterval), OperationTimeout: time.Duration(cfg.OperationTimeout),
		PlayerInterval: time.Duration(cfg.MetricsPlayerInterval),
		PlayerDetails:  cfg.MetricsPlayerDetails, ServerDirectory: cfg.ServerDirectory}
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
	return m.cfg.Clone(), configMetadata{Generation: m.generation, SHA256: m.digest, Source: m.path}
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
		{"MC_LAUNCH_EXECUTABLE", old.LaunchExecutable, next.LaunchExecutable},
		{"MC_LAUNCH_ARGS", old.LaunchArgs, next.LaunchArgs},
	}
	var changed []string
	for _, f := range fields {
		if !reflect.DeepEqual(f.old, f.next) {
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

func (m *runtimeConfig) reload(ctx context.Context, expected string, requestedPath ...string) (reloadResult, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.shuttingDown.Load() {
		return reloadResult{}, errors.New("gateway is shutting down")
	}
	if len(requestedPath) > 0 && requestedPath[0] != "" {
		if err := sameConfigSource(m.path, requestedPath[0]); err != nil {
			return reloadResult{}, err
		}
	}
	next, digest, err := readConfigSnapshot(m.path)
	if err != nil {
		return reloadResult{}, fmt.Errorf("reload rejected: %w", err)
	}
	if expected != "" && digest != expected {
		return reloadResult{}, errors.New(".env changed after the request was prepared; run task config:reload again")
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
	var prepared *telemetry.Prepared
	if m.metrics != nil {
		prepared, err = m.metrics.Prepare(metricsConfig(next))
		if err != nil {
			return reloadResult{}, fmt.Errorf("reload rejected: %w", err)
		}
		defer prepared.Abort()
	}
	err = m.controller.ReconfigureWith(ctx, lifecycle, func() error {
		if m.shuttingDown.Load() {
			return errors.New("gateway is shutting down")
		}
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
		m.cfg = next.Clone()
		m.digest = digest
		m.generation++
		metadata = configMetadata{Generation: m.generation, SHA256: m.digest, Source: m.path}
		m.mu.Unlock()
		if prepared != nil {
			prepared.Commit()
		}
		return nil
	})
	if err != nil {
		return reloadResult{}, err
	}
	m.log.Info("configuration_applied", "generation", metadata.Generation, "changed", changed)
	return reloadResult{configMetadata: metadata, Applied: true, Changed: changed}, nil
}

// A reload may name the source explicitly, but cannot silently switch the
// daemon to a second .env that a future service restart would not read.
func sameConfigSource(current, requested string) error {
	if !filepath.IsAbs(requested) {
		return errors.New("reload .env path must be absolute")
	}
	canonicalCurrent, err := filepath.EvalSymlinks(current)
	if err != nil {
		return fmt.Errorf("inspect active configuration source: %w", err)
	}
	canonicalRequested, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return fmt.Errorf("inspect requested configuration source: %w", err)
	}
	if filepath.Clean(canonicalCurrent) == filepath.Clean(canonicalRequested) {
		return nil
	}
	currentInfo, currentErr := os.Stat(current)
	requestedInfo, requestedErr := os.Stat(requested)
	if currentErr == nil && requestedErr == nil && os.SameFile(currentInfo, requestedInfo) {
		return nil
	}
	return errors.New("reload .env must be the daemon's configured source (symlink aliases are allowed); changing the source requires a gateway restart")
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
		return nil, errors.New(".env changed during validation; run task gateway:restart again")
	}
	old, metadata := m.snapshot()
	if old.RCONPassword != next.RCONPassword {
		return nil, errors.New("restart refused: apply MC_RCON_PASSWORD using task config:apply while Minecraft is stopped before restarting the gateway")
	}
	return map[string]any{"valid": true, "configuration": metadata, "restart_fields": restartFields(old, next)}, nil
}
