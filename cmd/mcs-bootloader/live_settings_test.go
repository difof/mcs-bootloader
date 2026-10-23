package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/telemetry"
)

func TestReloadExplicitSourceAcceptsAliasAndRejectsDifferentFile(t *testing.T) {
	f := newRuntimeFixture(t, false)
	_, original := f.m.snapshot()
	f.write(t, map[string]string{"MC_OVERRIDE_MOTD": "true", "MC_MOTD": "New message\nSecond line", "MC_RESTART_MESSAGE": "Gateway maintenance; please reconnect", "MC_RESTART_GRACE": "7s", "MC_WHITELIST_MESSAGE": "Ask for an invite", "MC_IDLE_TIMEOUT": "4h"})
	data, err := os.ReadFile(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(f.m.path), "other.env")
	if err := os.WriteFile(other, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{other, ".env"} {
		if _, err := f.m.reload(context.Background(), digest, invalid); err == nil {
			t.Fatal("reload silently changed to a different/relative source")
		}
	}
	if _, current := f.m.snapshot(); current != original {
		t.Fatal("rejected source changed the applied generation")
	}
	alias := filepath.Join(filepath.Dir(f.m.path), "source-alias.env")
	if err := os.Symlink(f.m.path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.reload(context.Background(), original.SHA256, alias); err == nil {
		t.Fatal("alias bypassed stale snapshot protection")
	}
	result, err := f.m.reload(context.Background(), digest, alias)
	if err != nil || !result.Applied || result.Source != f.m.path {
		t.Fatalf("canonical alias reload failed: %+v, %v", result, err)
	}
	cfg, _ := f.m.snapshot()
	traffic := f.m.gateway.ConfigSnapshot()
	if !cfg.OverrideMOTD || cfg.MOTD != "New message\nSecond line" || !traffic.OverrideMOTD || traffic.MOTD != cfg.MOTD || traffic.RestartMessage != cfg.RestartMessage || traffic.WhitelistMessage != cfg.WhitelistMessage || time.Duration(cfg.RestartGrace) != 7*time.Second || time.Duration(cfg.IdleTimeout) != 4*time.Hour {
		t.Fatal("live display/restart/idle fields did not commit together")
	}
}

func TestSocketReloadBindsExplicitPathAndPermissions(t *testing.T) {
	f := newRuntimeFixture(t, false)
	startRuntimeSocket(t, f)
	f.write(t, map[string]string{"MC_MOTD": "Changed"})
	_, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(reloadRequest{ExpectedSHA256: digest, EnvPath: filepath.Join(filepath.Dir(f.m.path), "missing.env")})
	status, _ := runtimeHTTPCall(t, f.socket, "POST", "reload", string(request))
	if status != http.StatusConflict {
		t.Fatal("socket ignored an invalid explicit source")
	}
	if err := os.Chmod(f.m.path, 0644); err != nil {
		t.Fatal(err)
	}
	request, _ = json.Marshal(reloadRequest{ExpectedSHA256: digest, EnvPath: f.m.path})
	status, _ = runtimeHTTPCall(t, f.socket, "POST", "reload", string(request))
	if status != http.StatusConflict {
		t.Fatal("socket explicit path bypassed private permissions")
	}
}

func TestMetricsBindFailureRejectsEveryReloadChange(t *testing.T) {
	f := newRuntimeFixture(t, true)
	f.m.metrics = telemetry.New(telemetry.Source{Snapshot: func() telemetry.Snapshot {
		return telemetry.Snapshot{Lifecycle: f.m.controller.Snapshot(), Traffic: f.m.gateway.Stats()}
	}}, "test", f.m.log)
	defer f.m.metrics.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	before, metadata := f.m.snapshot()
	traffic := f.m.gateway.ConfigSnapshot()
	f.write(t, map[string]string{"MC_METRICS_ENABLED": "true", "MC_METRICS_LISTEN": occupied.Addr().String(), "MC_OVERRIDE_MOTD": "true", "MC_MOTD": "Must not apply", "MC_IDLE_TIMEOUT": "3h", "MC_LOG_LEVEL": "debug"})
	if _, err := f.m.reload(context.Background(), ""); err == nil {
		t.Fatal("occupied metrics listener was accepted")
	}
	after, current := f.m.snapshot()
	if !reflect.DeepEqual(before, after) || metadata != current || !reflect.DeepEqual(traffic, f.m.gateway.ConfigSnapshot()) {
		t.Fatal("metrics bind failure partially applied other settings")
	}
	f.b.mu.Lock()
	updates := len(f.b.updates)
	f.b.mu.Unlock()
	if updates != 0 {
		t.Fatal("metrics bind failure changed the RCON session")
	}
}

func TestPrivateMetricsUsesAppliedCacheAndDoesNotWakeMinecraft(t *testing.T) {
	f := newRuntimeFixture(t, false)
	f.m.metrics = telemetry.New(telemetry.Source{Snapshot: func() telemetry.Snapshot {
		return telemetry.Snapshot{Lifecycle: f.m.controller.Snapshot(), Traffic: f.m.gateway.Stats()}
	}, Command: f.b.Command}, "test", f.m.log)
	defer f.m.metrics.Close()
	startRuntimeSocket(t, f)
	status, body := runtimeHTTPCall(t, f.socket, "GET", "metrics", "")
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "MC_METRICS_ENABLED=true") {
		t.Fatal("disabled metrics did not explain how to enable the applied endpoint")
	}
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := port.Addr().String()
	port.Close()
	f.write(t, map[string]string{"MC_METRICS_ENABLED": "true", "MC_METRICS_LISTEN": address})
	if _, err := f.m.reload(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// Pending disk edits cannot alter this applied socket operation.
	f.write(t, map[string]string{"MC_METRICS_ENABLED": "false", "MC_METRICS_LISTEN": "127.0.0.1:1"})
	status, body = runtimeHTTPCall(t, f.socket, "GET", "metrics", "")
	if status != http.StatusOK || !strings.Contains(string(body), "# HELP ") || !strings.Contains(string(body), "# TYPE ") {
		t.Fatalf("private metrics did not return cached exposition: status=%d", status)
	}
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if f.b.starts != 0 || f.b.stops != 0 || len(f.b.commands) != 0 {
		t.Fatal("scraping metrics woke or contacted an asleep Minecraft server")
	}
}

func TestControlledShutdownUsesNativeRCONAndAppliedReason(t *testing.T) {
	f := newRuntimeFixture(t, true)
	f.write(t, map[string]string{"MC_RESTART_MESSAGE": "Gateway updating; reconnect after a moment", "MC_RESTART_GRACE": "100ms"})
	if _, err := f.m.reload(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	f.m.drainForShutdown(f.cancelLifecycle, f.lifecycleDone)
	f.b.mu.Lock()
	commands := append([]runtimeCommandCall(nil), f.b.commands...)
	starts, stops := f.b.starts, f.b.stops
	f.b.mu.Unlock()
	if len(commands) != 1 || commands[0].command != "kick @a Gateway updating; reconnect after a moment" || commands[0].timeout != 250*time.Millisecond || starts != 0 || stops != 0 {
		t.Fatalf("shutdown changed lifecycle or bypassed native reason: %+v", commands)
	}
	if _, err := f.m.command(context.Background(), "list", time.Second); err == nil {
		t.Fatal("shutdown accepted a new operator command")
	}
	if _, err := f.m.reload(context.Background(), ""); err == nil {
		t.Fatal("shutdown accepted a configuration reload")
	}
}

func TestShutdownRCONFailureIsBoundedAndEmptyBackendIsNotKicked(t *testing.T) {
	t.Run("asleep", func(t *testing.T) {
		f := newRuntimeFixture(t, false)
		f.m.drainForShutdown(f.cancelLifecycle, f.lifecycleDone)
		f.b.mu.Lock()
		defer f.b.mu.Unlock()
		if len(f.b.commands) != 0 || f.b.starts != 0 || f.b.stops != 0 {
			t.Fatal("gateway shutdown contacted an asleep Minecraft server")
		}
	})
	t.Run("unresponsive RCON", func(t *testing.T) {
		f := newRuntimeFixture(t, true)
		f.write(t, map[string]string{"MC_OPERATION_TIMEOUT": "30ms", "MC_RESTART_GRACE": "20ms"})
		if _, err := f.m.reload(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		f.b.mu.Lock()
		f.b.commandHook = func(ctx context.Context, _ string, _ time.Duration) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		f.b.mu.Unlock()
		started := time.Now()
		f.m.drainForShutdown(f.cancelLifecycle, f.lifecycleDone)
		if time.Since(started) > time.Second {
			t.Fatal("unresponsive RCON defeated the configured operation timeout")
		}
		if !strings.Contains(f.log.String(), "gateway_restart_notice_failed") || strings.Contains(f.log.String(), runtimeTestSecret) {
			t.Fatal("shutdown failure log missing or leaked credential")
		}
	})
}
