package telemetry

import (
	"context"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func (m *Manager) collect(ctx context.Context, cfg Config, old collection) collection {
	c := newCollection()
	for k, v := range old.Health {
		v.Expected = false
		v.Success = false
		c.Health[k] = v
	}
	c.At = time.Now()
	c.attempt("self", m.selfPoints)
	if ctx.Err() != nil {
		return c
	}
	c.attempt("whitelist", func() ([]point, error) { return whitelistPoints(cfg) })
	var snap Snapshot
	if m.source.Snapshot != nil {
		snap = m.source.Snapshot()
	}
	snap = m.collectMinecraft(ctx, cfg, &c, old, snap)
	if ctx.Err() != nil {
		return c
	}
	if m.source.Status != nil && snap.Lifecycle.State == control.Ready {
		c.attempt("minecraft_status", func() ([]point, error) { return statusPoints(m.source.Status()) })
	}
	if snap.Lifecycle.BackendKnown && snap.Lifecycle.BackendRunning && m.source.PID != nil {
		m.collectProcess(&c)
	}
	return c
}

func (m *Manager) collectMinecraft(ctx context.Context, cfg Config, c *collection, old collection, snap Snapshot) Snapshot {
	if snap.Lifecycle.State != control.Ready || !snap.Lifecycle.BackendKnown || !snap.Lifecycle.BackendRunning || m.source.Command == nil {
		return snap
	}
	var players []player
	c.attempt("minecraft_players", func() ([]point, error) {
		var points []point
		var err error
		points, players, err = m.queryPlayers(ctx, cfg)
		return points, err
	})
	if ctx.Err() != nil {
		return snap
	}
	// Recheck the lifecycle: a manual stop may have begun after list.
	snap = m.source.Snapshot()
	if snap.Lifecycle.State == control.Ready {
		c.attempt("minecraft_ticks", func() ([]point, error) {
			text, err := m.source.Command(ctx, "tick query", cfg.OperationTimeout)
			if err != nil {
				return nil, err
			}
			return tickPoints(text)
		})
	}
	if ctx.Err() == nil && cfg.PlayerDetails && c.Health["minecraft_players"].Success && len(players) > 0 {
		m.collectPlayerDetails(ctx, cfg, c, old, players)
	}
	return snap
}

func (m *Manager) queryPlayers(ctx context.Context, cfg Config) ([]point, []player, error) {
	command := "list"
	if cfg.PlayerDetails {
		command = "list uuids"
	}
	text, err := m.source.Command(ctx, command, cfg.OperationTimeout)
	if err != nil {
		return nil, nil, err
	}
	count, capacity, players, err := parsePlayers(text, cfg.PlayerDetails)
	if err != nil {
		return nil, nil, err
	}
	return playerListPoints(count, capacity, players, cfg.PlayerDetails), players, nil
}
