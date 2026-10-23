package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/telemetry"
)

func (m *runtimeConfig) reload(ctx context.Context, expected string, requestedPath ...string) (reloadResult, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	next, digest, err := m.readReloadSnapshot(expected, requestedPath)
	if err != nil {
		return reloadResult{}, err
	}
	old, metadata := m.snapshot()
	if digest == metadata.SHA256 {
		return reloadResult{configMetadata: metadata, Changed: []string{}}, nil
	}
	lifecycle, traffic, err := validateReload(old, next)
	if err != nil {
		return reloadResult{}, err
	}
	var prepared *telemetry.Prepared
	if m.metrics != nil {
		prepared, err = m.metrics.Prepare(metricsConfig(next))
		if err != nil {
			return reloadResult{}, fmt.Errorf("reload rejected: %w", err)
		}
		defer prepared.Abort()
	}
	err = m.controller.ReconfigureWith(ctx, lifecycle, func() error {
		var commitErr error
		metadata, commitErr = m.applyReload(old, next, digest, traffic, prepared)
		return commitErr
	})
	if err != nil {
		return reloadResult{}, err
	}
	changed := changedFields(old, next)
	m.log.Info("configuration_applied", "generation", metadata.Generation, "changed", changed)
	return reloadResult{configMetadata: metadata, Applied: true, Changed: changed}, nil
}

func (m *runtimeConfig) readReloadSnapshot(expected string, requestedPath []string) (backend.Config, string, error) {
	if m.shuttingDown.Load() {
		return backend.Config{}, "", errors.New("gateway is shutting down")
	}
	if len(requestedPath) > 0 && requestedPath[0] != "" {
		if err := sameConfigSource(m.path, requestedPath[0]); err != nil {
			return backend.Config{}, "", err
		}
	}
	next, digest, err := readConfigSnapshot(m.path)
	if err != nil {
		return backend.Config{}, "", fmt.Errorf("reload rejected: %w", err)
	}
	if expected != "" && digest != expected {
		return backend.Config{}, "", errors.New(".env changed after the request was prepared; run mcs-bootloader config reload again")
	}
	return next, digest, nil
}

func validateReload(old, next backend.Config) (control.Config, gateway.Config, error) {
	if fields := restartFields(old, next); len(fields) > 0 {
		return control.Config{}, gateway.Config{}, fmt.Errorf("reload rejected; these settings require a gateway restart: %s", strings.Join(fields, ", "))
	}
	lifecycle, traffic := lifecycleConfig(next), trafficConfig(next)
	if err := lifecycle.Validate(); err != nil {
		return lifecycle, traffic, err
	}
	if err := traffic.Validate(); err != nil {
		return lifecycle, traffic, err
	}
	return lifecycle, traffic, nil
}

// applyReload runs on the lifecycle worker, excluding starts and stops while
// the related settings change. Every fallible check precedes the commit.
func (m *runtimeConfig) applyReload(old, next backend.Config, digest string, traffic gateway.Config, metrics *telemetry.Prepared) (configMetadata, error) {
	if m.shuttingDown.Load() {
		return configMetadata{}, errors.New("gateway is shutting down")
	}
	if err := m.checkPasswordRotation(old, next); err != nil {
		return configMetadata{}, err
	}
	// Reconfigure has already been validated and pinned addresses match.
	// There are no fallible commits after this operation succeeds.
	if err := m.gateway.Reconfigure(traffic); err != nil {
		return configMetadata{}, err
	}
	m.backend.ReconfigureRCON(next.RCONPassword, time.Duration(next.OperationTimeout), old.RCONPassword != next.RCONPassword)
	m.level.Set(parseLogLevel(next.LogLevel))
	m.mu.Lock()
	m.cfg = next.Clone()
	m.digest = digest
	m.generation++
	metadata := configMetadata{Generation: m.generation, SHA256: m.digest, Source: m.path}
	m.mu.Unlock()
	if metrics != nil {
		metrics.Commit()
	}
	return metadata, nil
}

func (m *runtimeConfig) checkPasswordRotation(old, next backend.Config) error {
	if old.RCONPassword == next.RCONPassword {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), time.Duration(old.OperationTimeout))
	defer cancel()
	running, err := m.backend.Running(checkCtx)
	if err != nil {
		return errors.New("cannot confirm Minecraft is stopped; RCON password was not changed")
	}
	if running {
		return errors.New("MC_RCON_PASSWORD can change only while Minecraft is stopped; restore its current value, or stop Minecraft before applying")
	}
	return nil
}
