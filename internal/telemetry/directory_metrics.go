package telemetry

import (
	"math"
	"time"
)

func (m *Manager) directoryPoints(cfg Config) []point {
	m.mu.RLock()
	samples := make(map[string]directorySample, len(m.directories))
	for name, sample := range m.directories {
		samples[name] = sample
	}
	m.mu.RUnlock()
	points := []point{
		gauge("mcs_metrics_directory_interval_seconds", "Delay between completed directory scan cycles.", cfg.DirectoryInterval.Seconds()),
		gauge("mcs_metrics_directory_timeout_seconds", "Cooperative time budget for each directory scan.", cfg.DirectoryTimeout.Seconds()),
		gauge("mcs_metrics_directories_configured", "Number of configured directory roots.", float64(len(cfg.Directories))),
	}
	now := time.Now()
	for name, sample := range samples {
		points = append(points,
			gauge("mcs_directory_scan_success", "Whether the last completed scan succeeded; inspect last-success age before using retained sizes.", boolean(sample.Success), "directory", name),
			gauge("mcs_directory_scan_in_progress", "Whether this directory is currently being scanned.", boolean(sample.InFlight), "directory", name),
			counter("mcs_directory_scan_errors_total", "Failed complete directory scans, excluding reload and shutdown cancellations.", float64(sample.Errors), "directory", name))
		if !sample.LastAttempt.IsZero() {
			points = append(points,
				gauge("mcs_directory_scan_last_attempt_timestamp_seconds", "Start time of the most recent directory scan attempt.", unix(sample.LastAttempt), "directory", name),
				gauge("mcs_directory_scan_duration_seconds", "Duration of the most recent completed scan attempt.", sample.Duration.Seconds(), "directory", name))
		}
		if !sample.LastSuccess.IsZero() {
			points = append(points,
				gauge("mcs_directory_scan_last_success_timestamp_seconds", "Completion time of the most recent complete successful directory scan.", unix(sample.LastSuccess), "directory", name),
				gauge("mcs_directory_scan_age_seconds", "Age of the last successful complete directory totals.", math.Max(0, now.Sub(sample.LastSuccess).Seconds()), "directory", name),
				gauge("mcs_directory_size_bytes", "Logical regular-file bytes from the last successful scan; hard links counted once per root.", float64(sample.Totals.Bytes), "directory", name),
				gauge("mcs_directory_allocated_bytes", "Allocated regular-file blocks times 512 from the last successful scan; excludes directory metadata.", float64(sample.Totals.Allocated), "directory", name),
				gauge("mcs_directory_files", "Unique regular files from the last successful scan.", float64(sample.Totals.Files), "directory", name),
				gauge("mcs_directory_directories", "Unique scanned directories, including the configured root.", float64(sample.Totals.Directories), "directory", name),
				gauge("mcs_directory_symlinks_skipped", "Symlinks excluded from the last successful scan.", float64(sample.Totals.Symlinks), "directory", name),
				gauge("mcs_directory_special_files_skipped", "Nonregular nondirectory files excluded from the last successful scan.", float64(sample.Totals.Special), "directory", name),
				gauge("mcs_directory_mounts_skipped", "Directories on another device excluded from the last successful scan.", float64(sample.Totals.Mounts), "directory", name))
		}
	}
	return points
}
