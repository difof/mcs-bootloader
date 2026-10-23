package control

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type State string

const (
	Unknown  State = "unknown"
	Asleep   State = "asleep"
	Starting State = "starting"
	Ready    State = "ready"
	Stopping State = "stopping"
	Failed   State = "failed"
)

type Snapshot struct {
	State                   State     `json:"state"`
	Players                 *int      `json:"players"`
	ActiveConnections       int       `json:"active_connections"`
	BackendRunning          bool      `json:"backend_running"`
	BackendKnown            bool      `json:"backend_known"`
	IdleSince               time.Time `json:"idle_since,omitempty"`
	StartedAt               time.Time `json:"started_at,omitempty"`
	StoppingSince           time.Time `json:"stopping_since,omitempty"`
	LastCheck               time.Time `json:"last_check,omitempty"`
	LastSuccessfulCheck     time.Time `json:"last_successful_check,omitempty"`
	NextWakeAt              time.Time `json:"next_wake_at,omitempty"`
	LastError               string    `json:"last_error,omitempty"`
	BootStartedAt           time.Time `json:"boot_started_at,omitempty"`
	ReadySince              time.Time `json:"ready_since,omitempty"`
	BootEstimateSeconds     float64   `json:"boot_estimate_seconds,omitempty"`
	LastBootDurationSeconds float64   `json:"last_boot_duration_seconds,omitempty"`
	SuccessfulBoots         uint64    `json:"successful_boots,omitempty"`
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snap
	if s.Players != nil {
		n := *s.Players
		s.Players = &n
	}
	return s
}

// AcquireJoin reserves a real connection before the gateway dials the backend.
// The reservation closes the otherwise dangerous player-check/join race.
func (c *Controller) AcquireJoin() (release func(), ok bool) {
	c.mu.Lock()
	if c.snap.State != Ready {
		c.mu.Unlock()
		return nil, false
	}
	c.snap.ActiveConnections++
	c.snap.IdleSince = time.Time{}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.snap.ActiveConnections--
			// A fresh successful RCON poll must establish emptiness again.
			c.snap.IdleSince = time.Time{}
			c.mu.Unlock()
		})
	}, true
}

func (c *Controller) CachedStatus() json.RawMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append(json.RawMessage(nil), c.status...)
}

// SetCachedStatus accepts only an object. Unknown/mod-provided fields remain
// opaque, and both input and output are copied to avoid shared mutable slices.
func (c *Controller) SetCachedStatus(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return errors.New("server status must be a JSON object")
	}
	c.mu.Lock()
	c.status = append(json.RawMessage(nil), raw...)
	c.mu.Unlock()
	return nil
}

func (c *Controller) update(state State, reason string, mutate func(*Snapshot)) {
	c.mu.Lock()
	before := c.snap.State
	previousPlayers := -1
	if c.snap.Players != nil {
		previousPlayers = *c.snap.Players
	}
	mutate(&c.snap)
	c.snap.State = state
	if state != Ready {
		c.snap.ReadySince = time.Time{}
	}
	players := -1
	if c.snap.Players != nil {
		players = *c.snap.Players
	}
	c.mu.Unlock()
	if before != state {
		c.logger.Info("state_changed", "from", before, "to", state, "reason", reason)
	}
	if players >= 0 && previousPlayers != players {
		c.logger.Info("players_changed", "count", players)
	}
}
