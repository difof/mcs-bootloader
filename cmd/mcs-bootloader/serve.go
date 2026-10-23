package main

import (
	"context"
	"errors"
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
	return serveWithOutput(configPath, newCLIOutput(os.Stdout, os.Stderr))
}

func serveWithOutput(configPath string, output *cliOutput) error {
	configPath, err := canonicalStartupConfig(configPath)
	if err != nil {
		return err
	}
	cfg, digest, err := readConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	log, level := daemonLogger(cfg.LogLevel, output)
	lock, err := lockStateDirectory(cfg.StateDirectory)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	// Signal notification must not cancel relays before Minecraft has delivered
	// its native disconnect. The controller has a separate cancellation path.
	ctx, cancelTraffic := context.WithCancel(context.Background())
	defer cancelTraffic()
	rc := rcon.NewPersistent(cfg.RCONAddress, cfg.RCONPassword, time.Duration(cfg.OperationTimeout))
	b := &backend.Systemd{Service: cfg.Service, Address: cfg.BackendAddress, Protocol: cfg.Protocol, RCON: rc}
	defer b.Close()
	if err := verifyStartupCredential(ctx, b, time.Duration(cfg.OperationTimeout)); err != nil {
		return err
	}
	c := control.New(lifecycleConfig(cfg), b, log)
	cachePath := filepath.Join(cfg.StateDirectory, "status.json")
	restoreCaches(c, cachePath, log)
	s := gateway.New(trafficConfig(cfg), c, log)
	runtime := &runtimeConfig{cfg: cfg.Clone(), path: configPath, digest: digest, generation: 1,
		controller: c, gateway: s, backend: b, level: level, log: log}
	daemon := gatewayDaemon{runtime: runtime, cachePath: cachePath, socketPath: cfg.AdminSocket}
	defer daemon.closeListeners()
	if err := daemon.listen(ctx, cfg); err != nil {
		return err
	}
	if err := runtime.startMetrics(cfg, b); err != nil {
		return err
	}
	defer runtime.metrics.Close()
	return daemon.run(ctx, signalCtx, cancelTraffic)
}

// gatewayDaemon owns the network listeners and worker lifetimes. Minecraft's
// independently supervised process remains outside this resource group.
type gatewayDaemon struct {
	runtime               *runtimeConfig
	traffic, admin        net.Listener
	adminHTTP             *http.Server
	cachePath, socketPath string
}

func (d *gatewayDaemon) run(ctx, signalCtx context.Context, cancelTraffic context.CancelFunc) error {
	m := d.runtime
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	defer cancelLifecycle()
	errCh := make(chan error, 2)
	lifecycleDone := make(chan struct{})
	go func() { defer close(lifecycleDone); m.controller.Run(lifecycleCtx) }()
	go func() { errCh <- m.gateway.Serve(ctx, d.traffic) }()
	go func() { errCh <- d.adminHTTP.Serve(d.admin) }()
	cacheDone := make(chan struct{})
	go func() { defer close(cacheDone); persistCache(ctx, m.controller, d.cachePath, m.log) }()
	cfg, _ := m.snapshot()
	m.log.Info("gateway_started", "version", version, "listen", cfg.Listen, "backend", cfg.BackendAddress, "idle_timeout", time.Duration(cfg.IdleTimeout).String(), "admin_socket", cfg.AdminSocket)
	var err error
	select {
	case <-signalCtx.Done():
		m.drainForShutdown(cancelLifecycle, lifecycleDone)
	case err = <-errCh:
		if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		// Startup/component failure is not an operator restart. It must never
		// kick unrelated players simply because this gateway failed to run.
		m.shuttingDown.Store(true)
		cancelLifecycle()
	}
	cancelTraffic()
	d.traffic.Close()
	m.gateway.Close()
	applied, _ := m.snapshot()
	shutdownCtx, done := context.WithTimeout(context.Background(), time.Duration(applied.OperationTimeout))
	defer done()
	d.adminHTTP.Shutdown(shutdownCtx)
	<-cacheDone
	m.log.Info("gateway_stopped", "backend_action", "left_under_separate_supervisor")
	return err
}
