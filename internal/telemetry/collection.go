package telemetry

import (
	"strings"
	"time"
)

var collectorNames = [...]string{"self", "whitelist", "minecraft_players", "minecraft_player_details", "minecraft_ticks", "minecraft_status", "jvm_process", "jvm_perfdata"}

type health struct {
	Expected, Success        bool
	LastAttempt, LastSuccess time.Time
	Errors                   uint64
	Duration                 time.Duration
}

type collection struct {
	Points                              map[string][]point
	Health                              map[string]health
	At                                  time.Time
	PID                                 int
	ProcessStart                        uint64
	Details                             map[string][]point
	DetailsPID                          int
	DetailsStart                        uint64
	DetailsAttempt                      time.Time
	DetailsNextPlayer, DetailsNextField int
}

func newCollection() collection {
	c := collection{Points: make(map[string][]point), Health: make(map[string]health), Details: make(map[string][]point)}
	for _, name := range collectorNames {
		c.Health[name] = health{}
	}
	return c
}
func (c collection) clone() collection {
	n := newCollection()
	n.At, n.PID, n.ProcessStart = c.At, c.PID, c.ProcessStart
	n.DetailsPID, n.DetailsStart, n.DetailsAttempt = c.DetailsPID, c.DetailsStart, c.DetailsAttempt
	n.DetailsNextPlayer, n.DetailsNextField = c.DetailsNextPlayer, c.DetailsNextField
	for k, v := range c.Details {
		n.Details[k] = v
	}
	for k, v := range c.Points {
		n.Points[k] = v
	} // immutable slices after publication
	for k, v := range c.Health {
		n.Health[k] = v
	}
	return n
}
func (c *collection) removeDetails() {
	c.Details = make(map[string][]point)
	c.DetailsAttempt = time.Time{}
	c.DetailsNextPlayer, c.DetailsNextField = 0, 0
	for k, points := range c.Points {
		var filtered []point
		for _, p := range points {
			if !strings.HasPrefix(p.Name, "mcs_minecraft_player_") && p.Name != "mcs_whitelist_player" {
				filtered = append(filtered, p)
			}
		}
		c.Points[k] = filtered
	}
}
func (c *collection) attempt(name string, fn func() ([]point, error)) {
	h := c.Health[name]
	h.Expected = true
	h.LastAttempt = time.Now()
	points, err := fn()
	h.Duration = time.Since(h.LastAttempt)
	h.Success = err == nil
	if err != nil {
		h.Errors++
	} else {
		h.LastSuccess = time.Now()
		c.Points[name] = points
	}
	c.Health[name] = h
}
