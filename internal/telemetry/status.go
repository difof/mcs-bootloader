package telemetry

import (
	"encoding/json"
	"errors"
)

func statusPoints(raw json.RawMessage) ([]point, error) {
	var s struct {
		Version struct {
			Name     *string `json:"name"`
			Protocol *int    `json:"protocol"`
		} `json:"version"`
		Players struct {
			Max *int `json:"max"`
		} `json:"players"`
		IsModded *bool `json:"isModded"`
		Forge    struct {
			Mods     []json.RawMessage `json:"mods"`
			Channels []json.RawMessage `json:"channels"`
		} `json:"forgeData"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Version.Name == nil || *s.Version.Name == "" || len(*s.Version.Name) > 256 || s.Version.Protocol == nil || *s.Version.Protocol < 0 || s.Players.Max == nil || *s.Players.Max < 0 {
		return nil, errors.New("cached native status lacks valid version or player capacity")
	}
	points := []point{gauge("mcs_minecraft_protocol_version", "Native cached status protocol version.", float64(*s.Version.Protocol)), gauge("mcs_minecraft_server_info", "Native cached status version information.", 1, "version", *s.Version.Name), gauge("mcs_minecraft_status_player_capacity", "Native cached status player capacity.", float64(*s.Players.Max))}
	if s.IsModded != nil {
		points = append(points, gauge("mcs_minecraft_modded", "Native cached status modded flag.", boolean(*s.IsModded)))
	}
	if s.Forge.Mods != nil {
		points = append(points, gauge("mcs_minecraft_advertised_mods", "Mod count advertised in cached Forge status.", float64(len(s.Forge.Mods))))
	}
	if s.Forge.Channels != nil {
		points = append(points, gauge("mcs_minecraft_advertised_channels", "Channel count advertised in cached Forge status.", float64(len(s.Forge.Channels))))
	}
	return points, nil
}
