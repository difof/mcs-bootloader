package gateway

import "sync/atomic"

// Stats are aggregate counters, intentionally replacing noisy per-ping and
// malformed-request log messages. They never include client payloads.
type Stats struct {
	Accepted         uint64 `json:"accepted"`
	CapacityRejected uint64 `json:"capacity_rejected"`
	InvalidRequests  uint64 `json:"invalid_requests"`
	StatusRequests   uint64 `json:"status_requests"`
	LoginRequests    uint64 `json:"login_requests"`
	WakeRequests     uint64 `json:"wake_requests"`
	WakeLimited      uint64 `json:"wake_limited"`
	Routed           uint64 `json:"routed"`
	BackendErrors    uint64 `json:"backend_errors"`
	WhitelistDenied  uint64 `json:"whitelist_denied"`
	WhitelistErrors  uint64 `json:"whitelist_errors"`
	DrainingRejected uint64 `json:"draining_rejected"`
	BytesToBackend   uint64 `json:"bytes_to_backend"`
	BytesToClient    uint64 `json:"bytes_to_client"`
}

type counters struct {
	accepted, capacityRejected, invalidRequests, statusRequests     atomic.Uint64
	loginRequests, wakeRequests, wakeLimited, routed, backendErrors atomic.Uint64
	whitelistDenied, whitelistErrors, drainingRejected              atomic.Uint64
	bytesToBackend, bytesToClient                                   atomic.Uint64
}

func (s *Server) Stats() Stats {
	return Stats{
		Accepted: s.stats.accepted.Load(), CapacityRejected: s.stats.capacityRejected.Load(),
		InvalidRequests: s.stats.invalidRequests.Load(), StatusRequests: s.stats.statusRequests.Load(),
		LoginRequests: s.stats.loginRequests.Load(), WakeRequests: s.stats.wakeRequests.Load(),
		WakeLimited: s.stats.wakeLimited.Load(), Routed: s.stats.routed.Load(), BackendErrors: s.stats.backendErrors.Load(),
		WhitelistDenied: s.stats.whitelistDenied.Load(), WhitelistErrors: s.stats.whitelistErrors.Load(), DrainingRejected: s.stats.drainingRejected.Load(),
		BytesToBackend: s.stats.bytesToBackend.Load(), BytesToClient: s.stats.bytesToClient.Load(),
	}
}
