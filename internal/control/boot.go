package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"
)

// Boot history is a disposable, bounded cache rather than operator
// configuration. A day is a defensive corruption limit, not a boot deadline.
const maxBootHistoryDuration = 24 * time.Hour

type bootHistory struct {
	Version             int     `json:"version"`
	EstimateSeconds     float64 `json:"estimate_seconds"`
	LastDurationSeconds float64 `json:"last_duration_seconds"`
	SuccessfulBoots     uint64  `json:"successful_boots"`
}

// processIdentifiedBackend returns the last inspected service invocation. It
// performs no I/O and changes for every new service process, including PID reuse.
type processIdentifiedBackend interface {
	ProcessIdentity() string
}

// observeProcess belongs to the lifecycle worker. Timing must never include a
// replacement process that the gateway did not itself start.
func (c *Controller) observeProcess() bool {
	identified, ok := c.backend.(processIdentifiedBackend)
	if !ok {
		return false
	}
	identity := identified.ProcessIdentity()
	if identity == "" {
		c.bootLearning = false
		return false
	}
	changed := c.processIdentity != "" && identity != c.processIdentity
	c.processIdentity = identity
	if changed {
		c.bootLearning = false
	}
	return changed
}

// Readiness probes can race an external service replacement. Recheck only
// when recording a new successful boot, and require the exact invocation that
// Start captured. An uncertain observation is discarded, never guessed.
func (c *Controller) verifyBootProcess(ctx context.Context) bool {
	identified, ok := c.backend.(processIdentifiedBackend)
	if !ok {
		return true
	}
	running, err := c.backend.Running(ctx)
	identity := identified.ProcessIdentity()
	if err == nil && running && identity != "" && identity == c.processIdentity {
		return true
	}
	c.bootLearning = false
	if err != nil {
		c.unhealthy(Unknown, "cannot determine backend state", err, true)
		return false
	}
	c.processIdentity = identity
	if !running {
		c.processIdentity = ""
		message := "backend exited while checking startup readiness"
		c.update(Failed, message, func(s *Snapshot) {
			s.BackendRunning, s.BackendKnown = false, true
			s.BootStartedAt, s.IdleSince = time.Time{}, time.Time{}
			s.Players = nil
			s.LastCheck, s.LastError = c.now(), message
			s.NextWakeAt = c.now().Add(c.cfg.WakeCooldown)
		})
		c.reportError(message)
		return false
	}
	c.update(Starting, "backend identity changed while checking startup readiness", func(s *Snapshot) {
		s.BackendRunning, s.BackendKnown = true, true
		s.StartedAt, s.BootStartedAt = c.now(), time.Time{}
		s.Players, s.IdleSince = nil, time.Time{}
		s.LastCheck, s.LastError = c.now(), ""
	})
	return false
}

func (c *Controller) recordSuccessfulBoot(now time.Time) {
	if !c.bootLearning {
		return
	}
	c.bootLearning = false
	c.mu.Lock()
	duration := now.Sub(c.snap.BootStartedAt)
	if c.snap.BootStartedAt.IsZero() || duration <= 0 || duration > maxBootHistoryDuration {
		c.mu.Unlock()
		return
	}
	seconds := duration.Seconds()
	c.snap.LastBootDurationSeconds = seconds
	if c.snap.BootEstimateSeconds <= 0 {
		c.snap.BootEstimateSeconds = seconds
	} else {
		// Smooth normal warm/cold-start variation while responding to pack
		// changes. A complete successful startup is the only observation.
		c.snap.BootEstimateSeconds = .75*c.snap.BootEstimateSeconds + .25*seconds
	}
	if c.snap.SuccessfulBoots < 1_000_000_000 {
		c.snap.SuccessfulBoots++
	}
	estimate := c.snap.BootEstimateSeconds
	c.mu.Unlock()
	c.logger.Info("backend_startup_measured", "duration", duration.Round(time.Second).String(), "estimate", (time.Duration(estimate * float64(time.Second))).Round(time.Second).String())
}

// RestoreBootHistory must run before Run. It restores only completed-duration
// observations, never an active timer or readiness. Adoption after a gateway
// restart cannot masquerade as a very short successful Minecraft boot.
func (c *Controller) RestoreBootHistory(data []byte) error {
	if c.started.Load() {
		return errors.New("boot history can only be restored before the controller starts")
	}
	if len(data) > 4096 {
		return errors.New("boot history exceeds size limit")
	}
	var cached bootHistory
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&cached) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid boot history")
	}
	validDuration := func(n float64) bool {
		return !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 && n <= maxBootHistoryDuration.Seconds()
	}
	if cached.Version != 1 || !validDuration(cached.EstimateSeconds) || !validDuration(cached.LastDurationSeconds) || cached.SuccessfulBoots == 0 || cached.SuccessfulBoots > 1_000_000_000 {
		return errors.New("invalid boot history values")
	}
	c.mu.Lock()
	c.snap.BootEstimateSeconds = cached.EstimateSeconds
	c.snap.LastBootDurationSeconds = cached.LastDurationSeconds
	c.snap.SuccessfulBoots = cached.SuccessfulBoots
	c.mu.Unlock()
	return nil
}

// CachedBootHistory copies the tiny cache in memory. Persistence is performed
// by the daemon's cache worker, never by login handling or a metrics scrape.
func (c *Controller) CachedBootHistory() []byte {
	c.mu.RLock()
	cached := bootHistory{Version: 1, EstimateSeconds: c.snap.BootEstimateSeconds,
		LastDurationSeconds: c.snap.LastBootDurationSeconds, SuccessfulBoots: c.snap.SuccessfulBoots}
	c.mu.RUnlock()
	if cached.SuccessfulBoots == 0 {
		return nil
	}
	data, _ := json.Marshal(cached)
	return data
}
