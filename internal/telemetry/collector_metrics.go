package telemetry

import (
	"math"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func (m *Manager) cachedPoints(cfg Config, cache collection, s control.Snapshot, now time.Time) []point {
	var points []point
	if !cache.At.IsZero() {
		points = append(points, gauge("mcs_metrics_collection_timestamp_seconds", "Start time of the latest published background collection.", unix(cache.At)), gauge("mcs_metrics_cache_age_seconds", "Age of the published background collection.", math.Max(0, now.Sub(cache.At).Seconds())))
	}
	for _, name := range collectorNames {
		h := cache.Health[name]
		expected, allow := m.collectorApplicable(name, cfg, cache, s)
		points = append(points, gauge("mcs_metrics_collector_expected", "Whether this collector should currently have data.", boolean(expected), "collector", name), gauge("mcs_metrics_collector_success", "Whether the latest collection succeeded and its backend state remains applicable.", boolean(h.Success && allow), "collector", name), counter("mcs_metrics_collector_errors_total", "Failed background collection attempts, excluding asleep or disabled collectors.", float64(h.Errors), "collector", name))
		if !h.LastAttempt.IsZero() {
			points = append(points, gauge("mcs_metrics_collector_last_attempt_timestamp_seconds", "Last attempt to collect this source.", unix(h.LastAttempt), "collector", name), gauge("mcs_metrics_collector_duration_seconds", "Duration of the most recent attempt.", h.Duration.Seconds(), "collector", name))
		}
		if !h.LastSuccess.IsZero() {
			points = append(points, gauge("mcs_metrics_collector_last_success_timestamp_seconds", "Most recent successful collection of this source.", unix(h.LastSuccess), "collector", name), gauge("mcs_metrics_collector_age_seconds", "Age of this source's most recent successful collection.", math.Max(0, now.Sub(h.LastSuccess).Seconds()), "collector", name))
		}
		if allow {
			points = append(points, cache.Points[name]...)
			if h.Success {
				points = append(points, processUptimePoints(name, cache.Points[name], now)...)
			}
		}
	}
	return points
}

func (m *Manager) collectorApplicable(name string, cfg Config, cache collection, s control.Snapshot) (bool, bool) {
	allow := true
	if strings.HasPrefix(name, "minecraft_") {
		allow = s.State == control.Ready && s.BackendKnown && s.BackendRunning
	}
	if name == "minecraft_player_details" && !cfg.PlayerDetails {
		allow = false
	}
	if strings.HasPrefix(name, "jvm_") {
		allow = s.BackendKnown && s.BackendRunning && m.source.PID != nil && m.source.PID() > 0
	}
	expected := cache.Health[name].Expected && allow
	if strings.HasPrefix(name, "jvm_") && allow && m.source.PID() != cache.PID {
		allow = false
	}
	if name == "minecraft_player_details" && m.source.PID != nil && m.source.PID() != cache.DetailsPID {
		allow = false
	}
	if strings.HasPrefix(name, "jvm_") && !s.StartedAt.IsZero() && cache.At.Before(s.StartedAt) {
		allow = false
	}
	return expected, allow
}
