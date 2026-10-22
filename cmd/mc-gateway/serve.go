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

	"mc-gateway/internal/backend"
	"mc-gateway/internal/control"
	"mc-gateway/internal/gateway"
	"mc-gateway/internal/rcon"
)

func serve(cfg backend.Config) error {
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	if cfg.LogLevel == "warn" {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	password, err := backend.ReadPassword(cfg.RCONPasswordFile)
	if err != nil {
		return err
	}
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rc := rcon.New(cfg.RCONAddress, password, time.Duration(cfg.OperationTimeout))
	b := &backend.Systemd{Service: cfg.Service, Address: cfg.BackendAddress, Protocol: cfg.Protocol, RCON: rc}
	c := control.New(control.Config{
		IdleTimeout: time.Duration(cfg.IdleTimeout), PollInterval: time.Duration(cfg.PollInterval), OperationTimeout: time.Duration(cfg.OperationTimeout),
		StartupTimeout: time.Duration(cfg.StartupTimeout), ShutdownWarnAfter: time.Duration(cfg.ShutdownWarnAfter), WakeCooldown: time.Duration(cfg.WakeCooldown),
	}, b, log)
	cachePath := filepath.Join(cfg.StateDirectory, "status.json")
	if cached, err := readCache(cachePath); err == nil {
		c.SetCachedStatus(cached)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("status_cache_unavailable", "error", err.Error())
	}
	fallback, _ := json.Marshal(map[string]any{
		"version":     map[string]any{"name": cfg.Version, "protocol": cfg.Protocol},
		"players":     map[string]any{"max": cfg.MaxPlayers, "online": 0, "sample": []any{}},
		"description": cfg.SleepingMOTD, "isModded": true,
	})
	s := gateway.New(gateway.Config{
		BackendAddress: cfg.BackendAddress, Protocol: cfg.Protocol, MaxConnections: cfg.MaxConnections,
		HandshakeTimeout: time.Duration(cfg.HandshakeTimeout), DialTimeout: time.Duration(cfg.DialTimeout),
		BootMessage: cfg.BootMessage, SleepingMOTD: cfg.SleepingMOTD, StartingMOTD: cfg.StartingMOTD, FailedMOTD: cfg.FailedMOTD, FallbackStatus: fallback,
	}, c, log)
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
	httpServer := &http.Server{Handler: adminHandler(c, s), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 4096}
	errCh := make(chan error, 2)
	go c.Run(ctx)
	go func() { errCh <- s.Serve(ctx, listener) }()
	go func() { errCh <- httpServer.Serve(admin) }()
	cacheDone := make(chan struct{})
	go func() { defer close(cacheDone); persistCache(ctx, c, cachePath, log) }()
	log.Info("gateway_started", "version", version, "listen", cfg.Listen, "backend", cfg.BackendAddress, "idle_timeout", time.Duration(cfg.IdleTimeout).String(), "admin_socket", cfg.AdminSocket)
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		cancel()
	}
	listener.Close()
	s.Close()
	shutdownCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	httpServer.Shutdown(shutdownCtx)
	<-cacheDone
	log.Info("gateway_stopped", "backend_action", "left_under_separate_supervisor")
	return err
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
