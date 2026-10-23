package main

import (
	"context"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

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
