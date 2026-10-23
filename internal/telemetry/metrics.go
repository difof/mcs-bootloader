package telemetry

import (
	"time"

	"github.com/difof/mcs-bootloader/internal/rcon"
)

func (m *Manager) points(cfg Config, cache collection, snapshot Snapshot, stats rcon.Stats) []point {
	now := time.Now()
	points := m.operatorPoints(cfg, cache, snapshot)
	points = append(points, controllerPoints(snapshot, now)...)
	points = append(points, gatewayPoints(snapshot.Traffic)...)
	points = append(points, rconPoints(stats)...)
	points = append(points, m.cachedPoints(cfg, cache, snapshot.Lifecycle, now)...)
	points = append(points, m.directoryPoints(cfg)...)
	return points
}

func (m *Manager) operatorPoints(cfg Config, cache collection, snapshot Snapshot) []point {
	points := []point{
		gauge("mcs_build_info", "Gateway build version.", 1, "version", m.version),
		gauge("mcs_configuration_generation", "Applied configuration generation.", float64(snapshot.ConfigGeneration)),
		gauge("mcs_metrics_enabled", "Whether metrics are enabled in the applied configuration.", boolean(cfg.Enabled)),
		gauge("mcs_metrics_poll_interval_seconds", "Applied background metrics interval.", cfg.PollInterval.Seconds()),
		gauge("mcs_metrics_player_interval_seconds", "Applied detailed player collection interval.", cfg.PlayerInterval.Seconds()),
		gauge("mcs_metrics_player_details_enabled", "Whether player identities and detail fields are exported.", boolean(cfg.PlayerDetails)),
		gauge("mcs_whitelist_known", "Whether the latest native whitelist read succeeded.", boolean(cache.Health["whitelist"].Success)),
		counter("mcs_metrics_http_requests_total", "Requests to the metrics HTTP listener.", float64(m.requests.Load())),
		counter("mcs_metrics_http_errors_total", "Unsupported paths or methods on the metrics HTTP listener.", float64(m.requestErrors.Load())),
		counter("mcs_metrics_http_duration_seconds_total", "Total HTTP metrics handling duration.", float64(m.requestNanos.Load())/1e9),
	}
	return points
}
