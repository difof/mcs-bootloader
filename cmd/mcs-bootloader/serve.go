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
)

func serve(configPath string) error {
	cfg, digest, err := readConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	level := new(slog.LevelVar)
	level.Set(parseLogLevel(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
	httpServer := &http.Server{Handler: adminHandler(c, s, runtime), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: maxCommandTimeout + 10*time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 4096}
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
