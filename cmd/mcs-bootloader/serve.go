package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/rcon"
	"github.com/difof/mcs-bootloader/internal/telemetry"
)

func serve(configPath string) error {
	return serveWithOutput(configPath, newCLIOutput(os.Stdout, os.Stderr))
}

func serveWithOutput(configPath string, output *cliOutput) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	// Keep the actual startup source stable across aliases and atomic edits.
	configPath, err = filepath.EvalSymlinks(configPath)
	if err != nil {
		return err
	}
	cfg, digest, err := readConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	level := new(slog.LevelVar)
	level.Set(parseLogLevel(cfg.LogLevel))
	var logOutput io.Writer = output.Out
	// Keep journal records structured even for existing installations whose unit
	// predates the explicit --json flag; foreground runs use human formatting.
	if !output.JSON && os.Getenv("INVOCATION_ID") == "" {
		logOutput = logWriter{output: output}
	}
	log := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: level}))
	password := cfg.RCONPassword
	if err := os.MkdirAll(cfg.StateDirectory, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDirectory, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another gateway owns this state directory: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	// Signal notification must not cancel relays before Minecraft has delivered
	// its native disconnect. The controller has a separate cancellation path.
	ctx, cancelTraffic := context.WithCancel(context.Background())
	defer cancelTraffic()
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	defer cancelLifecycle()
	rc := rcon.NewPersistent(cfg.RCONAddress, password, time.Duration(cfg.OperationTimeout))
	b := &backend.Systemd{Service: cfg.Service, Address: cfg.BackendAddress, Protocol: cfg.Protocol, RCON: rc}
	defer b.Close()
	// A direct service restart must not adopt a known-wrong edited password
	// while a live Java process still uses its original credential. Network
	// unavailability during normal boot remains the controller's concern.
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Duration(cfg.OperationTimeout))
	if running, inspectErr := b.Running(probeCtx); inspectErr == nil && running {
		if _, probeErr := rc.Players(probeCtx); errors.Is(probeErr, rcon.ErrAuthentication) {
			probeCancel()
			return fmt.Errorf("MC_RCON_PASSWORD does not authenticate to the running Minecraft server; restore its current value before restarting the gateway")
		}
	}
	probeCancel()

	c := control.New(lifecycleConfig(cfg), b, log)
	cachePath := filepath.Join(cfg.StateDirectory, "status.json")
	if cached, err := readCache(cachePath); err == nil {
		c.SetCachedStatus(cached)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("status_cache_unavailable", "error", err.Error())
	}
	s := gateway.New(trafficConfig(cfg), c, log)
	runtime := &runtimeConfig{cfg: cfg.Clone(), path: configPath, digest: digest, generation: 1,
		controller: c, gateway: s, backend: b, level: level, log: log}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	if err := os.MkdirAll(filepath.Dir(cfg.AdminSocket), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(cfg.AdminSocket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("admin socket path exists and is not a socket")
		}
		if err := os.Remove(cfg.AdminSocket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	admin, err := net.Listen("unix", cfg.AdminSocket)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	defer admin.Close()
	defer os.Remove(cfg.AdminSocket)
	if err := os.Chmod(cfg.AdminSocket, 0600); err != nil {
		return err
	}
	httpServer := &http.Server{Handler: adminHandler(c, s, runtime), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: maxCommandTimeout + 10*time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 4096,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	runtime.metrics = telemetry.New(telemetry.Source{
		Snapshot: func() telemetry.Snapshot {
			applied, metadata := runtime.snapshot()
			return telemetry.Snapshot{Lifecycle: c.Snapshot(), Traffic: s.Stats(), IdleTimeout: time.Duration(applied.IdleTimeout), ConfigGeneration: metadata.Generation}
		}, Command: b.Command, PID: b.MainPID, RCONStats: b.RCONStats, Status: c.CachedStatus,
	}, version, log)
	defer runtime.metrics.Close()
	prepared, err := runtime.metrics.Prepare(metricsConfig(cfg))
	if err != nil {
		return fmt.Errorf("metrics startup: %w", err)
	}
	prepared.Commit()
	errCh := make(chan error, 2)
	lifecycleDone := make(chan struct{})
	go func() { defer close(lifecycleDone); c.Run(lifecycleCtx) }()
	go func() { errCh <- s.Serve(ctx, listener) }()
	go func() { errCh <- httpServer.Serve(admin) }()
	cacheDone := make(chan struct{})
	go func() { defer close(cacheDone); persistCache(ctx, c, cachePath, log) }()
	log.Info("gateway_started", "version", version, "listen", cfg.Listen, "backend", cfg.BackendAddress, "idle_timeout", time.Duration(cfg.IdleTimeout).String(), "admin_socket", cfg.AdminSocket)
	select {
	case <-signalCtx.Done():
		runtime.drainForShutdown(cancelLifecycle, lifecycleDone)
	case err = <-errCh:
		if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		// Startup/component failure is not an operator restart. It must never
		// kick unrelated players simply because this gateway failed to run.
		runtime.shuttingDown.Store(true)
		cancelLifecycle()
	}
	cancelTraffic()
	listener.Close()
	s.Close()
	applied, _ := runtime.snapshot()
	shutdownCtx, done := context.WithTimeout(context.Background(), time.Duration(applied.OperationTimeout))
	defer done()
	httpServer.Shutdown(shutdownCtx)
	<-cacheDone
	log.Info("gateway_stopped", "backend_action", "left_under_separate_supervisor")
	return err
}

// drainForShutdown preserves the established byte streams until the backend
// itself sends each player's disconnect. It never stops or starts Minecraft.
// Abrupt process death cannot deliver a message; controlled SIGTERM/SIGINT can.
func (m *runtimeConfig) drainForShutdown(cancelLifecycle context.CancelFunc, lifecycleDone <-chan struct{}) {
	m.shuttingDown.Store(true)
	active := m.gateway.BeginDrain()
	cancelLifecycle()
	cfg, _ := m.snapshot()
	workerCtx, workerCancel := context.WithTimeout(context.Background(), time.Duration(cfg.OperationTimeout))
	workerStopped := false
	select {
	case <-lifecycleDone:
		workerStopped = true
	case <-workerCtx.Done():
	}
	workerCancel()
	if m.metrics != nil {
		_ = m.metrics.Close()
	}
	snapshot := m.controller.Snapshot()
	playersPresent := snapshot.Players != nil && *snapshot.Players > 0
	messageSent := false
	if workerStopped && snapshot.State != control.Stopping && (active > 0 || playersPresent) {
		commandCtx, commandCancel := context.WithTimeout(context.Background(), time.Duration(cfg.OperationTimeout))
		_, err := m.backend.Command(commandCtx, "kick @a "+cfg.RestartMessage, time.Duration(cfg.OperationTimeout))
		commandCancel()
		if err != nil {
			// Do not expose arbitrary RCON text or config values in the journal.
			m.log.Warn("gateway_restart_notice_failed", "error", "Minecraft did not acknowledge the native disconnect before the operation timeout")
		} else {
			messageSent = true
		}
	}
	graceCtx, graceCancel := context.WithTimeout(context.Background(), time.Duration(cfg.RestartGrace))
	drained := m.gateway.WaitForRelays(graceCtx) == nil
	graceCancel()
	m.log.Info("gateway_drained", "active_relays_at_start", active, "native_disconnect_sent", messageSent, "relays_drained", drained, "lifecycle_stopped", workerStopped)
}

func readCache(path string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2*1024*1024 {
		return nil, fmt.Errorf("status cache exceeds size limit")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("invalid status cache")
	}
	return b, nil
}

func persistCache(ctx context.Context, c *control.Controller, path string, log *slog.Logger) {
	timer := time.NewTicker(10 * time.Second)
	defer timer.Stop()
	var last []byte
	var lastError string
	write := func() {
		data := c.CachedStatus()
		if len(data) == 0 || bytes.Equal(last, data) {
			return
		}
		if err := atomicWrite(path, data); err != nil {
			if err.Error() != lastError {
				log.Warn("status_cache_write_failed", "error", err.Error())
				lastError = err.Error()
			}
			return
		}
		if lastError != "" {
			log.Info("status_cache_recovered")
			lastError = ""
		}
		last = append(last[:0], data...)
	}
	for {
		select {
		case <-ctx.Done():
			write()
			return
		case <-timer.C:
			write()
		}
	}
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".status-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
