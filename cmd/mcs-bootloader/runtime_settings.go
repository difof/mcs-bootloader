package main

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/telemetry"
)

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
		PlayerDetails:  cfg.MetricsPlayerDetails, ServerDirectory: cfg.ServerDirectory,
		Directories: cfg.MetricsDirectories, DirectoryInterval: time.Duration(cfg.MetricsDirectoryInterval), DirectoryTimeout: time.Duration(cfg.MetricsDirectoryTimeout)}
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
