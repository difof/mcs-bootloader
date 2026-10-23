package telemetry

import (
	"math"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

// These calculations use lifecycle snapshots and verified, already-cached
// process timestamps only. Scraping must never inspect procfs or query RCON.
func lifecycleTimingPoints(s control.Snapshot, now time.Time) []point {
	known := s.SuccessfulBoots > 0 && finite(s.BootEstimateSeconds) && s.BootEstimateSeconds > 0
	points := []point{
		gauge("mcs_startup_estimate_known", "Whether a learned estimate from successful complete boots is available.", boolean(known)),
		counter("mcs_startup_successes_total", "Complete boots observed by this controller; persisted in the boot history cache.", float64(s.SuccessfulBoots)),
	}
	if known {
		points = append(points, gauge("mcs_startup_estimate_seconds", "Smoothed duration of previous successful complete boots; an estimate, not a deadline.", s.BootEstimateSeconds))
	}
	if s.SuccessfulBoots > 0 && finite(s.LastBootDurationSeconds) && s.LastBootDurationSeconds > 0 {
		points = append(points, gauge("mcs_startup_duration_seconds", "Duration of the most recently observed complete successful boot, including readiness polling delay.", s.LastBootDurationSeconds))
	}
	if s.State == control.Starting && !s.BootStartedAt.IsZero() {
		elapsed := math.Max(0, now.Sub(s.BootStartedAt).Seconds())
		points = append(points,
			gauge("mcs_startup_started_timestamp_seconds", "Time the gateway initiated the current startup attempt.", unix(s.BootStartedAt)),
			gauge("mcs_startup_elapsed_seconds", "Elapsed time since the gateway initiated the current startup attempt.", elapsed),
		)
		if known {
			points = append(points,
				gauge("mcs_startup_remaining_seconds", "Estimated seconds remaining; zero after the estimate is exceeded, not a readiness signal.", math.Max(0, s.BootEstimateSeconds-elapsed)),
				gauge("mcs_startup_estimate_overdue", "Whether the current startup has exceeded its learned estimate.", boolean(elapsed >= s.BootEstimateSeconds)),
			)
		}
	}
	if s.State == control.Ready && s.BackendKnown && s.BackendRunning && !s.ReadySince.IsZero() {
		points = append(points,
			gauge("mcs_controller_ready_since_timestamp_seconds", "Time this controller observed the current ready period; gateway restarts begin a new observation.", unix(s.ReadySince)),
			gauge("mcs_controller_ready_duration_seconds", "Duration of this controller's current observed ready period; not Java process uptime.", math.Max(0, now.Sub(s.ReadySince).Seconds())),
		)
	}
	return points
}

// The JVM timestamp is the process start read from procfs, not the lifecycle
// controller's adoption time. Consequently a gateway restart cannot reset the
// server's uptime. The caller applies the same PID/state guards as JVM samples.
func processUptimePoints(collector string, cached []point, now time.Time) []point {
	var source, metric string
	switch collector {
	case "jvm_process":
		source, metric = "mcs_jvm_process_start_time_seconds", "mcs_minecraft_uptime_seconds"
	case "self":
		source, metric = "process_start_time_seconds", "process_uptime_seconds"
	default:
		return nil
	}
	for _, sample := range cached {
		if sample.Name != source || !finite(sample.Value) || sample.Value <= 0 || sample.Value > unix(now) {
			continue
		}
		points := []point{gauge(metric, "Elapsed process uptime derived from the latest verified procfs start timestamp, including startup and shutdown time.", unix(now)-sample.Value)}
		if collector == "jvm_process" {
			points = append(points, gauge("mcs_minecraft_start_time_seconds", "Unix start time of the verified Java backend process; unchanged by gateway restarts.", sample.Value))
		}
		return points
	}
	return nil
}
