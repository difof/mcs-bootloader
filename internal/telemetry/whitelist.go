package telemetry

import (
	"github.com/difof/mcs-bootloader/internal/whitelist"
)

func whitelistPoints(cfg Config) ([]point, error) {
	s, err := whitelist.Snapshot(cfg.ServerDirectory)
	if err != nil {
		return nil, err
	}
	points := []point{gauge("mcs_whitelist_enabled", "Native Minecraft whitelist enabled flag.", boolean(s.Enabled))}
	if s.Enabled {
		points = append(points, gauge("mcs_whitelist_entries", "Entries in Minecraft's native whitelist.", float64(len(s.Entries))))
		if cfg.PlayerDetails {
			for _, e := range s.Entries {
				points = append(points, gauge("mcs_whitelist_player", "Current native whitelist membership; enabled only with player details.", 1, "name", e.Name, "uuid", e.UUID))
			}
		}
	}
	return points, nil
}
