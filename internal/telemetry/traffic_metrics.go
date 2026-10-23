package telemetry

import (
	"strconv"

	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

func gatewayPoints(t gateway.Stats) []point {
	var points []point
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
