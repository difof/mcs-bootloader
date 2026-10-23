package telemetry

import (
	"math"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func controllerPoints(snapshot Snapshot, now time.Time) []point {
	s := snapshot.Lifecycle
	points := []point{
		gauge("mcs_controller_ready", "Whether the controller currently admits new backend connections.", boolean(s.State == control.Ready)),
		gauge("mcs_backend_known", "Whether the last lifecycle service-state inspection is known.", boolean(s.BackendKnown)),
		gauge("mcs_controller_players_known", "Whether the lifecycle controller has a known player count.", boolean(s.Players != nil)),
		gauge("mcs_gateway_active_connections", "Active or pending backend join reservations.", float64(s.ActiveConnections)),
		gauge("mcs_controller_error", "Whether the lifecycle snapshot currently contains an error; error text is never a label.", boolean(s.LastError != "")),
		gauge("mcs_idle_timeout_seconds", "Applied empty-server idle timeout.", snapshot.IdleTimeout.Seconds()),
	}
	for _, state := range []control.State{control.Unknown, control.Asleep, control.Starting, control.Ready, control.Stopping, control.Failed} {
		points = append(points, gauge("mcs_controller_state", "Current lifecycle state as a one-hot gauge.", boolean(s.State == state), "state", string(state)))
	}
	points = append(points, lifecycleTimingPoints(s, now)...)
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
	return points
}
