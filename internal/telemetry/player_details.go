package telemetry

import (
	"context"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func (m *Manager) collectPlayerDetails(ctx context.Context, cfg Config, c *collection, old collection, players []player) {
	if m.source.PID == nil {
		return
	}
	pid := m.source.PID()
	start, err := processIdentity("/proc", pid)
	if err != nil {
		c.attempt("minecraft_player_details", func() ([]point, error) { return nil, err })
		return
	}
	c.DetailsPID, c.DetailsStart = pid, start
	if old.detailsFresh(pid, start, cfg.PlayerInterval, players) {
		c.reusePlayerDetails(old, players)
		return
	}
	c.DetailsAttempt = time.Now()
	firstPlayer, firstField := c.detailBatchOrder(old, pid, start, len(players))
	budget, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	defer cancel()
	failed := false
	for playerOffset := range players {
		p := players[(firstPlayer+playerOffset)%len(players)]
		points, complete := m.playerDetailPoints(budget, cfg.OperationTimeout, pid, p, firstField)
		points = append(points,
			gauge("mcs_minecraft_player_details_available", "All requested detail fields succeeded for this player in the last detail batch.", boolean(complete), "name", p.Name, "uuid", p.UUID),
			gauge("mcs_minecraft_player_details_attempt_timestamp_seconds", "Unix time of the latest player detail attempt.", unix(c.DetailsAttempt), "name", p.Name, "uuid", p.UUID))
		c.Details[p.UUID] = points
		c.Points["minecraft_player_details"] = append(c.Points["minecraft_player_details"], points...)
		failed = failed || !complete
	}
	c.finishPlayerDetails(failed)
	end, err := processIdentity("/proc", pid)
	if err != nil || end != start || m.source.PID() != pid {
		c.Details = make(map[string][]point)
		delete(c.Points, "minecraft_player_details")
		h := c.Health["minecraft_player_details"]
		h.Success = false
		c.Health["minecraft_player_details"] = h
	}
}

func (old collection) detailsFresh(pid int, start uint64, interval time.Duration, players []player) bool {
	if old.DetailsPID != pid || old.DetailsStart != start || old.DetailsAttempt.IsZero() || time.Since(old.DetailsAttempt) >= interval {
		return false
	}
	for _, p := range players {
		if _, ok := old.Details[p.UUID]; !ok {
			return false
		}
	}
	return true
}

func (c *collection) reusePlayerDetails(old collection, players []player) {
	c.DetailsAttempt = old.DetailsAttempt
	c.DetailsNextPlayer, c.DetailsNextField = old.DetailsNextPlayer, old.DetailsNextField
	h := old.Health["minecraft_player_details"]
	h.Expected = true
	c.Health["minecraft_player_details"] = h
	for _, p := range players {
		if points, ok := old.Details[p.UUID]; ok {
			c.Details[p.UUID] = points
			c.Points["minecraft_player_details"] = append(c.Points["minecraft_player_details"], points...)
		}
	}
}

func (c *collection) detailBatchOrder(old collection, pid int, start uint64, count int) (firstPlayer, firstField int) {
	if old.DetailsPID == pid && old.DetailsStart == start {
		firstPlayer, firstField = old.DetailsNextPlayer, old.DetailsNextField
	}
	if count > 0 {
		firstPlayer %= count
		c.DetailsNextPlayer = (firstPlayer + 1) % count
	}
	firstField %= len(playerFields)
	c.DetailsNextField = firstField
	// Advance the field only after every player has led a batch. Advancing
	// both on each batch can couple their cycles and starve half the pairs.
	if c.DetailsNextPlayer == 0 {
		c.DetailsNextField = (firstField + 1) % len(playerFields)
	}
	return firstPlayer, firstField
}

func (m *Manager) playerDetailPoints(ctx context.Context, timeout time.Duration, pid int, p player, firstField int) ([]point, bool) {
	var points []point
	complete := true
	for fieldOffset := range playerFields {
		field := (firstField + fieldOffset) % len(playerFields)
		f := playerFields[field]
		if ctx.Err() != nil || m.source.PID() != pid || m.source.Snapshot().Lifecycle.State != control.Ready {
			return points, false
		}
		// Names come exclusively from the strictly validated list uuids
		// response. Each command requests one fixed scalar/list field.
		text, err := m.source.Command(ctx, "data get entity "+p.Name+" "+f.Path, timeout)
		if err != nil {
			complete = false
			continue
		}
		fieldPoints, err := playerFieldPoints(p, field, text)
		if err != nil {
			complete = false
			continue
		}
		points = append(points, fieldPoints...)
		points = append(points, gauge("mcs_minecraft_player_field_timestamp_seconds", "Unix time of the last successful read of this player field.", unix(time.Now()), "name", p.Name, "uuid", p.UUID, "field", f.Path))
	}
	return points, complete
}

func (c *collection) finishPlayerDetails(failed bool) {
	h := c.Health["minecraft_player_details"]
	h.Expected = true
	h.LastAttempt = c.DetailsAttempt
	h.Duration = time.Since(h.LastAttempt)
	h.Success = !failed
	if failed {
		h.Errors++
	} else {
		h.LastSuccess = time.Now()
	}
	c.Health["minecraft_player_details"] = h
}
