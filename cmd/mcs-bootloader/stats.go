package main

import (
	"strings"
	"time"
)

// stats is deliberately a bounded operational overview. The complete exported
// sample set remains available from metrics --raw or metrics --json.
func (o *cliOutput) Stats(raw []byte) error {
	samples, _, err := parseMetrics(raw)
	if err != nil {
		return err
	}
	groups := map[string]any{}
	set := func(group, key string, value any) {
		current, ok := groups[group].(map[string]any)
		if !ok {
			current = map[string]any{}
			groups[group] = current
		}
		current[key] = value
	}
	fields := map[string][2]string{
		"mcs_controller_players":                {"gateway", "players"},
		"mcs_gateway_active_connections":        {"gateway", "active_connections"},
		"mcs_idle_remaining_seconds":            {"gateway", "idle_remaining_seconds"},
		"mcs_metrics_cache_age_seconds":         {"gateway", "metrics_age_seconds"},
		"process_uptime_seconds":                {"gateway", "uptime_seconds"},
		"mcs_controller_ready_duration_seconds": {"gateway", "observed_ready_seconds"},
		"process_resident_memory_bytes":         {"gateway", "resident_memory_bytes"},
		"go_goroutines":                         {"gateway", "goroutines"},
		"mcs_rcon_connected":                    {"rcon", "connected"},
		"mcs_rcon_in_flight":                    {"rcon", "in_flight"},
		"mcs_rcon_queued":                       {"rcon", "queued"},
		"mcs_rcon_connection_errors_total":      {"rcon", "connection_errors_total"},
		"mcs_minecraft_players_online":          {"minecraft", "players_online"},
		"mcs_minecraft_players_capacity":        {"minecraft", "players_capacity"},
		"mcs_minecraft_uptime_seconds":          {"minecraft", "uptime_seconds"},
		"mcs_startup_estimate_known":            {"startup", "estimate_known"},
		"mcs_startup_estimate_seconds":          {"startup", "estimate_seconds"},
		"mcs_startup_duration_seconds":          {"startup", "last_duration_seconds"},
		"mcs_startup_successes_total":           {"startup", "successful_boots"},
		"mcs_startup_elapsed_seconds":           {"startup", "elapsed_seconds"},
		"mcs_startup_remaining_seconds":         {"startup", "remaining_seconds"},
		"mcs_startup_estimate_overdue":          {"startup", "estimate_overdue"},
		"mcs_minecraft_tick_target_rate_hertz":  {"minecraft", "target_tick_rate_hertz"},
		"mcs_minecraft_tick_duration_seconds":   {"minecraft", "mean_tick_duration_seconds"},
		"mcs_whitelist_known":                   {"minecraft", "whitelist_known"},
		"mcs_whitelist_enabled":                 {"minecraft", "whitelist_enabled"},
		"mcs_whitelist_entries":                 {"minecraft", "whitelist_entries"},
		"mcs_jvm_process_pid":                   {"jvm", "pid"},
		"mcs_jvm_process_resident_memory_bytes": {"jvm", "resident_memory_bytes"},
		"mcs_jvm_process_virtual_memory_bytes":  {"jvm", "virtual_memory_bytes"},
		"mcs_jvm_process_cpu_seconds_total":     {"jvm", "cpu_seconds_total"},
		"mcs_jvm_threads_live":                  {"jvm", "threads_live"},
	}
	directoryFields := map[string]string{
		"mcs_directory_size_bytes":         "logical_size_bytes",
		"mcs_directory_allocated_bytes":    "allocated_bytes",
		"mcs_directory_files":              "files",
		"mcs_directory_scan_success":       "scan_success",
		"mcs_directory_sample_age_seconds": "sample_age_seconds",
		"mcs_directory_scan_age_seconds":   "sample_age_seconds",
	}
	collectors := map[string]any{}
	for _, sample := range samples {
		if field, ok := fields[sample.Name]; ok {
			set(field[0], field[1], sample.Value)
		}
		if sample.Name == "mcs_controller_state" && sample.Value == float64(1) {
			set("gateway", "state", sample.Labels["state"])
		}
		if sample.Name == "mcs_metrics_collector_success" {
			collectors[sample.Labels["collector"]] = sample.Value
		}
		if sample.Name == "mcs_gateway_bytes_total" {
			set("gateway", sample.Labels["direction"]+"_bytes", sample.Value)
		}
		if field, ok := directoryFields[sample.Name]; ok {
			directories, ok := groups["directories"].(map[string]any)
			if !ok {
				directories = map[string]any{}
				groups["directories"] = directories
			}
			name := sample.Labels["directory"]
			values, ok := directories[name].(map[string]any)
			if !ok {
				values = map[string]any{}
				directories[name] = values
			}
			values[field] = sample.Value
		}
	}
	if len(collectors) > 0 {
		groups["collector_success"] = collectors
	}
	groups["note"] = "Cached samples; unavailable fields are omitted. Use metrics for the complete sample set."
	if !o.JSON {
		humanizeStats(groups)
	}
	return o.Result("Statistics", groups)
}
func humanizeStats(values map[string]any) {
	for key, value := range values {
		if child, ok := value.(map[string]any); ok {
			humanizeStats(child)
			continue
		}
		number, ok := value.(float64)
		if !ok {
			continue
		}
		if strings.HasSuffix(key, "_bytes") {
			values[key] = formatBytes(number)
		}
		if humanDurationKey(key) && number >= 0 && number < float64(1<<63-1)/float64(time.Second) {
			values[key] = (time.Duration(number * float64(time.Second))).Round(time.Second).String()
		}
	}
}

func humanDurationKey(key string) bool {
	switch key {
	case "uptime_seconds", "observed_ready_seconds", "idle_remaining_seconds", "metrics_age_seconds", "sample_age_seconds", "estimate_seconds", "last_duration_seconds", "elapsed_seconds", "remaining_seconds":
		return true
	default:
		return false
	}
}
func formatBytes(value float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	return humanValue(value) + " " + units[index]
}
