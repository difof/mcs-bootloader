package telemetry

import (
	"bytes"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

type label struct{ Name, Value string }
type point struct {
	Name, Family, Help, Kind string
	Value                    float64
	Labels                   []label
}

func gauge(name, help string, value float64, labels ...string) point {
	p := point{Name: name, Family: name, Help: help, Kind: "gauge", Value: value}
	for i := 0; i+1 < len(labels); i += 2 {
		p.Labels = append(p.Labels, label{labels[i], labels[i+1]})
	}
	sort.Slice(p.Labels, func(i, j int) bool { return p.Labels[i].Name < p.Labels[j].Name })
	return p
}
func counter(name, help string, value float64, labels ...string) point {
	p := gauge(name, help, value, labels...)
	p.Kind = "counter"
	return p
}

func escapeHelp(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n").Replace(strings.ToValidUTF8(s, "�"))
}
func escapeLabel(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(strings.ToValidUTF8(s, "�"))
}
func labelsText(labels []label) string {
	if len(labels) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(l.Name)
		out.WriteString("=\"")
		out.WriteString(escapeLabel(l.Value))
		out.WriteByte('"')
	}
	out.WriteByte('}')
	return out.String()
}

// encode emits Prometheus0.0.4 text, grouping each family under one HELP/TYPE
// declaration and escaping the three reserved label characters explicitly.
func encode(points []point) []byte {
	families := make(map[string][]point)
	for _, p := range points {
		families[p.Family] = append(families[p.Family], p)
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, name := range names {
		family := families[name]
		sort.SliceStable(family, func(i, j int) bool {
			a, b := family[i], family[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if a.Kind == "histogram" && strings.HasSuffix(a.Name, "_bucket") {
				var x, y float64
				for _, l := range a.Labels {
					if l.Name == "le" {
						x, _ = strconv.ParseFloat(l.Value, 64)
					}
				}
				for _, l := range b.Labels {
					if l.Name == "le" {
						y, _ = strconv.ParseFloat(l.Value, 64)
					}
				}
				if x != y {
					return x < y
				}
			}
			return labelsText(a.Labels) < labelsText(b.Labels)
		})
		out.WriteString("# HELP " + name + " " + escapeHelp(family[0].Help) + "\n# TYPE " + name + " " + family[0].Kind + "\n")
		for _, p := range family {
			out.WriteString(p.Name)
			out.WriteString(labelsText(p.Labels))
			out.WriteByte(' ')
			out.WriteString(strconv.FormatFloat(p.Value, 'g', -1, 64))
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func (m *Manager) points(cfg Config, cache collection, snapshot Snapshot, stats rcon.Stats) []point {
	now := time.Now()
	s := snapshot.Lifecycle
	points := []point{
		gauge("mcs_build_info", "Gateway build version.", 1, "version", m.version),
		gauge("mcs_configuration_generation", "Applied configuration generation.", float64(snapshot.ConfigGeneration)),
		gauge("mcs_metrics_enabled", "Whether metrics are enabled in the applied configuration.", boolean(cfg.Enabled)),
		gauge("mcs_metrics_poll_interval_seconds", "Applied background metrics interval.", cfg.PollInterval.Seconds()),
		gauge("mcs_metrics_player_interval_seconds", "Applied detailed player collection interval.", cfg.PlayerInterval.Seconds()),
		gauge("mcs_metrics_player_details_enabled", "Whether player identities and detail fields are exported.", boolean(cfg.PlayerDetails)),
		gauge("mcs_controller_ready", "Whether the controller currently admits new backend connections.", boolean(s.State == control.Ready)),
		gauge("mcs_backend_known", "Whether the last lifecycle service-state inspection is known.", boolean(s.BackendKnown)),
		gauge("mcs_controller_players_known", "Whether the lifecycle controller has a known player count.", boolean(s.Players != nil)),
		gauge("mcs_whitelist_known", "Whether the latest native whitelist read succeeded.", boolean(cache.Health["whitelist"].Success)),
		gauge("mcs_gateway_active_connections", "Active or pending backend join reservations.", float64(s.ActiveConnections)),
		gauge("mcs_controller_error", "Whether the lifecycle snapshot currently contains an error; error text is never a label.", boolean(s.LastError != "")),
		gauge("mcs_idle_timeout_seconds", "Applied empty-server idle timeout.", snapshot.IdleTimeout.Seconds()),
		counter("mcs_metrics_http_requests_total", "Requests to the metrics HTTP listener.", float64(m.requests.Load())),
		counter("mcs_metrics_http_errors_total", "Unsupported paths or methods on the metrics HTTP listener.", float64(m.requestErrors.Load())),
		counter("mcs_metrics_http_duration_seconds_total", "Total HTTP metrics handling duration.", float64(m.requestNanos.Load())/1e9),
	}
	for _, state := range []control.State{control.Unknown, control.Asleep, control.Starting, control.Ready, control.Stopping, control.Failed} {
		points = append(points, gauge("mcs_controller_state", "Current lifecycle state as a one-hot gauge.", boolean(s.State == state), "state", string(state)))
	}
	if s.BackendKnown {
		points = append(points, gauge("mcs_backend_running", "Whether the supervised backend service is running.", boolean(s.BackendRunning)))
	}
	if s.Players != nil {
		points = append(points, gauge("mcs_controller_players", "Player count used by the lifecycle safety checks.", float64(*s.Players)))
	}
	for _, v := range []struct {
		name, help string
		t          time.Time
	}{
		{"mcs_controller_last_check_timestamp_seconds", "Last lifecycle health-check attempt.", s.LastCheck},
		{"mcs_controller_last_success_timestamp_seconds", "Last successful lifecycle health check.", s.LastSuccessfulCheck},
		{"mcs_backend_started_timestamp_seconds", "Controller's recorded backend start time.", s.StartedAt},
		{"mcs_backend_stopping_timestamp_seconds", "Time the controller entered stopping.", s.StoppingSince},
		{"mcs_next_wake_timestamp_seconds", "Earliest next automatic wake allowed by the controller.", s.NextWakeAt},
		{"mcs_idle_since_timestamp_seconds", "Time the controller first confirmed emptiness for this idle period.", s.IdleSince},
	} {
		if !v.t.IsZero() {
			points = append(points, gauge(v.name, v.help, unix(v.t)))
		}
	}
	if !s.LastSuccessfulCheck.IsZero() {
		points = append(points, gauge("mcs_controller_check_age_seconds", "Age of the last successful lifecycle health check.", math.Max(0, now.Sub(s.LastSuccessfulCheck).Seconds())))
	}
	if !s.IdleSince.IsZero() && s.State == control.Ready {
		deadline := s.IdleSince.Add(snapshot.IdleTimeout)
		points = append(points, gauge("mcs_idle_deadline_timestamp_seconds", "Idle deadline, subject to the next safe lifecycle check.", unix(deadline)), gauge("mcs_idle_remaining_seconds", "Seconds until the current idle deadline.", math.Max(0, deadline.Sub(now).Seconds())))
	}
	t := snapshot.Traffic
	for _, v := range []struct {
		name, help string
		value      uint64
	}{
		{"accepted_connections", "TCP connections accepted by the gateway.", t.Accepted},
		{"capacity_rejections", "Connections rejected by the gateway capacity limit.", t.CapacityRejected},
		{"invalid_requests", "Rejected invalid or unsupported handshakes.", t.InvalidRequests},
		{"status_requests", "Minecraft status requests received.", t.StatusRequests},
		{"login_requests", "Minecraft login requests received.", t.LoginRequests},
		{"wake_requests", "Automatic wake requests from login attempts.", t.WakeRequests},
		{"wake_limited", "Wake requests refused by rate limiting.", t.WakeLimited},
		{"routed_connections", "Connections routed to the backend.", t.Routed},
		{"backend_errors", "Gateway backend connection or relay failures.", t.BackendErrors},
		{"whitelist_denied", "Login attempts denied by the native whitelist.", t.WhitelistDenied},
		{"whitelist_errors", "Login attempts rejected because native whitelist state was unreadable.", t.WhitelistErrors},
		{"draining_rejections", "Admissions rejected during controlled shutdown.", t.DrainingRejected},
	} {
		points = append(points, counter("mcs_gateway_"+v.name+"_total", v.help, float64(v.value)))
	}
	points = append(points, counter("mcs_gateway_bytes_total", "Relayed opaque game bytes, including successfully replayed login frames.", float64(t.BytesToBackend), "direction", "to_backend"), counter("mcs_gateway_bytes_total", "Relayed opaque game bytes, including successfully replayed login frames.", float64(t.BytesToClient), "direction", "to_client"))
	points = append(points, rconPoints(stats)...)
	if !cache.At.IsZero() {
		points = append(points, gauge("mcs_metrics_collection_timestamp_seconds", "Start time of the latest published background collection.", unix(cache.At)), gauge("mcs_metrics_cache_age_seconds", "Age of the published background collection.", math.Max(0, now.Sub(cache.At).Seconds())))
	}
	for _, name := range collectorNames {
		h := cache.Health[name]
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
		expected := h.Expected && allow
		if strings.HasPrefix(name, "jvm_") && allow && m.source.PID() != cache.PID {
			allow = false
		}
		if name == "minecraft_player_details" && m.source.PID != nil && m.source.PID() != cache.DetailsPID {
			allow = false
		}
		if strings.HasPrefix(name, "jvm_") && !s.StartedAt.IsZero() && cache.At.Before(s.StartedAt) {
			allow = false
		}
		points = append(points, gauge("mcs_metrics_collector_expected", "Whether this collector should currently have data.", boolean(expected), "collector", name), gauge("mcs_metrics_collector_success", "Whether the latest collection succeeded and its backend state remains applicable.", boolean(h.Success && allow), "collector", name), counter("mcs_metrics_collector_errors_total", "Failed background collection attempts, excluding asleep or disabled collectors.", float64(h.Errors), "collector", name))
		if !h.LastAttempt.IsZero() {
			points = append(points, gauge("mcs_metrics_collector_last_attempt_timestamp_seconds", "Last attempt to collect this source.", unix(h.LastAttempt), "collector", name), gauge("mcs_metrics_collector_duration_seconds", "Duration of the most recent attempt.", h.Duration.Seconds(), "collector", name))
		}
		if !h.LastSuccess.IsZero() {
			points = append(points, gauge("mcs_metrics_collector_last_success_timestamp_seconds", "Most recent successful collection of this source.", unix(h.LastSuccess), "collector", name), gauge("mcs_metrics_collector_age_seconds", "Age of this source's most recent successful collection.", math.Max(0, now.Sub(h.LastSuccess).Seconds()), "collector", name))
		}
		if allow {
			points = append(points, cache.Points[name]...)
		}
	}
	return points
}

func rconPoints(stats rcon.Stats) []point {
	points := []point{gauge("mcs_rcon_connected", "Whether the shared RCON client currently holds a TCP connection.", boolean(stats.Connected)), gauge("mcs_rcon_in_flight", "RCON commands currently executing or waiting for the shared gate.", float64(stats.InFlight)), gauge("mcs_rcon_queued", "RCON commands currently waiting for the shared gate.", float64(stats.Queued))}
	for _, v := range []struct {
		name, help string
		value      uint64
	}{
		{"connection_attempts", "RCON TCP connection attempts.", stats.ConnectionAttempts},
		{"connections", "Successful RCON TCP connections.", stats.Connections},
		{"connection_errors", "Failed RCON TCP connection attempts.", stats.ConnectionErrors},
		{"reconnects", "Successful RCON connections after the first.", stats.Reconnects},
		{"authentication_attempts", "RCON authentication exchanges attempted.", stats.AuthenticationAttempts},
		{"authentication_successes", "Successful RCON authentication exchanges.", stats.AuthenticationSuccesses},
		{"authentication_errors", "Failed or incomplete RCON authentication exchanges.", stats.AuthenticationErrors},
	} {
		points = append(points, counter("mcs_rcon_"+v.name+"_total", v.help, float64(v.value)))
	}
	for _, op := range stats.Operations {
		for _, v := range []struct {
			name, help string
			value      uint64
		}{
			{"requests", "Completed RCON requests including validation and queueing failures.", op.Requests},
			{"successes", "Successfully completed RCON requests.", op.Successes},
			{"errors", "Failed RCON requests.", op.Errors},
			{"timeouts", "RCON requests ending in a deadline or network timeout.", op.Timeouts},
			{"cancellations", "RCON requests canceled by their caller.", op.Canceled},
			{"response_bytes", "Bytes in successful RCON response bodies; contents are never exported.", op.ResponseBytes},
		} {
			points = append(points, counter("mcs_rcon_"+v.name+"_total", v.help, float64(v.value), "operation", op.Operation))
		}
		const family = "mcs_rcon_request_duration_seconds"
		const help = "RCON request duration including shared-gate queueing, authentication and response collection."
		for i, bound := range rcon.DurationBounds {
			p := gauge(family+"_bucket", help, float64(op.DurationBuckets[i]), "operation", op.Operation, "le", strconv.FormatFloat(bound, 'g', -1, 64))
			p.Family, p.Kind = family, "histogram"
			points = append(points, p)
		}
		p := gauge(family+"_bucket", help, float64(op.Requests), "operation", op.Operation, "le", "+Inf")
		p.Family, p.Kind = family, "histogram"
		points = append(points, p)
		p = gauge(family+"_count", help, float64(op.Requests), "operation", op.Operation)
		p.Family, p.Kind = family, "histogram"
		points = append(points, p)
		p = gauge(family+"_sum", help, op.DurationSeconds, "operation", op.Operation)
		p.Family, p.Kind = family, "histogram"
		points = append(points, p)
	}
	return points
}
