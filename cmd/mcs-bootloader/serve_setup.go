package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/rcon"
	"github.com/difof/mcs-bootloader/internal/telemetry"
)

func canonicalStartupConfig(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Keep the actual startup source stable across aliases and atomic edits.
	return filepath.EvalSymlinks(absolute)
}

func daemonLogger(logLevel string, output *cliOutput) (*slog.Logger, *slog.LevelVar) {
	level := new(slog.LevelVar)
	level.Set(parseLogLevel(logLevel))
	var writer io.Writer = output.Out
	// Keep journal records structured even for existing installations whose unit
	// predates the explicit --json flag; foreground runs use human formatting.
	if !output.JSON && os.Getenv("INVOCATION_ID") == "" {
		writer = logWriter{output: output}
	}
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})), level
}

func lockStateDirectory(path string) (*os.File, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(path, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another gateway owns this state directory: %w", err)
	}
	return lock, nil
}

func verifyStartupCredential(ctx context.Context, b *backend.Systemd, timeout time.Duration) error {
	// A direct service restart must not adopt a known-wrong edited password
	// while a live Java process still uses its original credential. Network
	// unavailability during normal boot remains the controller's concern.
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if running, inspectErr := b.Running(probeCtx); inspectErr == nil && running {
		if _, probeErr := b.RCON.Players(probeCtx); errors.Is(probeErr, rcon.ErrAuthentication) {
			return errors.New("MC_RCON_PASSWORD does not authenticate to the running Minecraft server; restore its current value before restarting the gateway")
		}
	}
	return nil
}

func (d *gatewayDaemon) listen(ctx context.Context, cfg backend.Config) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	d.traffic = listener
	admin, err := listenAdmin(cfg.AdminSocket)
	if err != nil {
		return err
	}
	d.admin = admin
	d.adminHTTP = &http.Server{
		Handler:           adminHandler(d.runtime.controller, d.runtime.gateway, d.runtime),
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: maxCommandTimeout + 10*time.Second, IdleTimeout: 15 * time.Second,
		MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	return nil
}

func listenAdmin(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("admin socket path exists and is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	admin, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("admin socket: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		os.Remove(path)
		admin.Close()
		return nil, err
	}
	return admin, nil
}

func (d *gatewayDaemon) closeListeners() {
	if d.admin != nil {
		os.Remove(d.socketPath)
		d.admin.Close()
	}
	if d.traffic != nil {
		d.traffic.Close()
	}
}

func (m *runtimeConfig) startMetrics(cfg backend.Config, b *backend.Systemd) error {
	m.metrics = telemetry.New(telemetry.Source{
		Snapshot: func() telemetry.Snapshot {
			applied, metadata := m.snapshot()
			return telemetry.Snapshot{Lifecycle: m.controller.Snapshot(), Traffic: m.gateway.Stats(), IdleTimeout: time.Duration(applied.IdleTimeout), ConfigGeneration: metadata.Generation}
		}, Command: b.Command, PID: b.MainPID, RCONStats: b.RCONStats, Status: m.controller.CachedStatus,
	}, version, m.log)
	prepared, err := m.metrics.Prepare(metricsConfig(cfg))
	if err != nil {
		m.metrics.Close()
		return fmt.Errorf("metrics startup: %w", err)
	}
	prepared.Commit()
	return nil
}
