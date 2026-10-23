package gateway

import (
	"encoding/json"
	"fmt"

	"github.com/difof/mcs-bootloader/internal/control"
)

// status edits only lifecycle display fields. Unknown top-level and nested
// fields are retained as raw JSON, including NeoForge/modpack extensions.
func (s *Server) status() json.RawMessage {
	return s.statusWithConfig(*s.cfg.Load())
}

func (s *Server) statusWithConfig(cfg Config) json.RawMessage {
	raw := s.controller.CachedStatus()
	var status map[string]json.RawMessage
	if json.Unmarshal(raw, &status) != nil || status == nil {
		if json.Unmarshal(cfg.FallbackStatus, &status) != nil || status == nil {
			status = map[string]json.RawMessage{
				"version":     json.RawMessage(fmt.Sprintf(`{"name":"Minecraft","protocol":%d}`, cfg.Protocol)),
				"players":     json.RawMessage(`{"max":20,"online":0}`),
				"description": json.RawMessage(`{"text":"Minecraft server"}`),
			}
		}
	}
	snapshot := s.controller.Snapshot()
	if snapshot.State == control.Ready {
		if snapshot.Players != nil {
			setPlayerCount(status, *snapshot.Players, false)
		}
	} else {
		message := cfg.FailedMOTD
		switch snapshot.State {
		case control.Asleep:
			message = cfg.SleepingMOTD
		case control.Starting:
			message = cfg.StartingMOTD
		case control.Stopping:
			message = "Server is saving and going to sleep. Please reconnect shortly."
		case control.Unknown:
			message = "Checking server health. Please reconnect shortly."
		}
		status["description"], _ = json.Marshal(map[string]string{"text": message})
		count := 0
		if snapshot.Players != nil {
			count = *snapshot.Players
		}
		setPlayerCount(status, count, true)
	}
	encoded, _ := json.Marshal(status)
	return encoded
}

func setPlayerCount(status map[string]json.RawMessage, count int, clearSample bool) {
	var players map[string]json.RawMessage
	if json.Unmarshal(status["players"], &players) != nil || players == nil {
		players = map[string]json.RawMessage{"max": json.RawMessage(`20`)}
	}
	players["online"], _ = json.Marshal(count)
	if clearSample {
		players["sample"] = json.RawMessage(`[]`)
	}
	status["players"], _ = json.Marshal(players)
}
