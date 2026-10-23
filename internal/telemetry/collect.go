package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/whitelist"
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

func (m *Manager) collect(ctx context.Context, cfg Config, old collection) collection {
	c := newCollection()
	for k, v := range old.Health {
		v.Expected = false
		v.Success = false
		c.Health[k] = v
	}
	c.At = time.Now()
	c.attempt("self", func() ([]point, error) { return m.selfPoints() })
	if ctx.Err() != nil {
		return c
	}
	c.attempt("whitelist", func() ([]point, error) {
		s, err := whitelist.Snapshot(cfg.ServerDirectory)
		if err != nil {
			return nil, err
		}
		points := []point{gauge("mcs_whitelist_enabled", "Native Minecraft whitelist enabled flag.", boolean(s.Enabled))}
		if s.Enabled {
			points = append(points, gauge("mcs_whitelist_entries", "Entries in Minecraft's native whitelist.", float64(len(s.Entries))))
			if cfg.PlayerDetails {
				for _, e := range s.Entries {
					points = append(points, gauge("mcs_whitelist_player", "Current native whitelist membership; enabled only with player details.", 1, "name", e.Name, "uuid", e.UUID))
				}
			}
		}
		return points, nil
	})
	var snap Snapshot
	var players []player
	if m.source.Snapshot != nil {
		snap = m.source.Snapshot()
	}
	if snap.Lifecycle.State == control.Ready && snap.Lifecycle.BackendKnown && snap.Lifecycle.BackendRunning && m.source.Command != nil {
		c.attempt("minecraft_players", func() ([]point, error) {
			command := "list"
			if cfg.PlayerDetails {
				command = "list uuids"
			}
			text, err := m.source.Command(ctx, command, cfg.OperationTimeout)
			if err != nil {
				return nil, err
			}
			_, _, players, err = parsePlayers(text, cfg.PlayerDetails)
			if err != nil {
				return nil, err
			}
			return playerPoints(text, cfg.PlayerDetails)
		})
		if ctx.Err() != nil {
			return c
		}
		// Recheck the lifecycle: a manual stop may have begun after list.
		snap = m.source.Snapshot()
		if snap.Lifecycle.State == control.Ready {
			c.attempt("minecraft_ticks", func() ([]point, error) {
				text, err := m.source.Command(ctx, "tick query", cfg.OperationTimeout)
				if err != nil {
					return nil, err
				}
				return tickPoints(text)
			})
		}
		if ctx.Err() == nil && cfg.PlayerDetails && c.Health["minecraft_players"].Success && len(players) > 0 {
			m.collectPlayerDetails(ctx, cfg, &c, old, players)
		}
	}
	if ctx.Err() != nil {
		return c
	}
	if m.source.Status != nil && snap.Lifecycle.State == control.Ready {
		c.attempt("minecraft_status", func() ([]point, error) { return statusPoints(m.source.Status()) })
	}
	if snap.Lifecycle.BackendKnown && snap.Lifecycle.BackendRunning && m.source.PID != nil {
		pid := m.source.PID()
		if pid > 0 {
			var proc processData
			c.attempt("jvm_process", func() ([]point, error) {
				var err error
				proc, err = readProcess("/proc", pid, m.clockTicks, m.bootTime)
				if err != nil {
					return nil, err
				}
				if err = verifyJavaProcess("/proc", pid); err != nil {
					return nil, err
				}
				c.PID, c.ProcessStart = pid, proc.StartTicks
				return proc.points("mcs_jvm_process"), nil
			})
			if c.Health["jvm_process"].Success {
				c.attempt("jvm_perfdata", func() ([]point, error) {
					perf, err := readPerfData("/proc", pid, proc)
					if err != nil {
						return nil, err
					}
					return perf.points(), nil
				})
				// Discard both sets if systemd changed its PID during collection.
				if m.source.PID() != pid {
					c.Points["jvm_process"] = nil
					c.Points["jvm_perfdata"] = nil
					for _, name := range []string{"jvm_process", "jvm_perfdata"} {
						h := c.Health[name]
						h.Success = false
						c.Health[name] = h
					}
				}
			}
		}
	}
	return c
}

func (m *Manager) collectPlayerDetails(ctx context.Context, cfg Config, c *collection, old collection, players []player) {
	if m.source.PID == nil {
		return
	}
	pid := m.source.PID()
	start, err := processIdentity("/proc", pid)
	if err != nil {
		c.attempt("minecraft_player_details", func() ([]point, error) { return nil, err })
		return
	}
	c.DetailsPID, c.DetailsStart = pid, start
	allCached := true
	for _, p := range players {
		if _, ok := old.Details[p.UUID]; !ok {
			allCached = false
			break
		}
	}
	if allCached && old.DetailsPID == pid && old.DetailsStart == start && !old.DetailsAttempt.IsZero() && time.Since(old.DetailsAttempt) < cfg.PlayerInterval {
		c.DetailsAttempt = old.DetailsAttempt
		c.DetailsNextPlayer, c.DetailsNextField = old.DetailsNextPlayer, old.DetailsNextField
		h := old.Health["minecraft_player_details"]
		h.Expected = true
		c.Health["minecraft_player_details"] = h
		for _, p := range players {
			if points, ok := old.Details[p.UUID]; ok {
				c.Details[p.UUID] = points
				c.Points["minecraft_player_details"] = append(c.Points["minecraft_player_details"], points...)
			}
		}
		return
	}
	c.DetailsAttempt = time.Now()
	firstPlayer, firstField := 0, 0
	if old.DetailsPID == pid && old.DetailsStart == start {
		firstPlayer, firstField = old.DetailsNextPlayer, old.DetailsNextField
	}
	if len(players) > 0 {
		firstPlayer %= len(players)
		c.DetailsNextPlayer = (firstPlayer + 1) % len(players)
	}
	firstField %= len(playerFields)
	c.DetailsNextField = firstField
	// Advance the field only after every player has led a batch. Advancing
	// both on each batch can couple their cycles and starve half the pairs.
	if c.DetailsNextPlayer == 0 {
		c.DetailsNextField = (firstField + 1) % len(playerFields)
	}
	budget, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	defer cancel()
	failed := false
	h := c.Health["minecraft_player_details"]
	h.Expected = true
	h.LastAttempt = c.DetailsAttempt
	for playerOffset := range players {
		p := players[(firstPlayer+playerOffset)%len(players)]
		var points []point
		complete := true
		for fieldOffset := range playerFields {
			field := (firstField + fieldOffset) % len(playerFields)
			f := playerFields[field]
			if budget.Err() != nil || m.source.PID() != pid || m.source.Snapshot().Lifecycle.State != control.Ready {
				failed = true
				complete = false
				break
			}
			// Names come exclusively from the strictly validated list uuids
			// response. Each command requests one fixed scalar/list field.
			text, err := m.source.Command(budget, "data get entity "+p.Name+" "+f.Path, cfg.OperationTimeout)
			if err != nil {
				failed = true
				complete = false
				continue
			}
			fieldPoints, err := playerFieldPoints(p, field, text)
			if err != nil {
				failed = true
				complete = false
				continue
			}
			points = append(points, fieldPoints...)
			points = append(points, gauge("mcs_minecraft_player_field_timestamp_seconds", "Unix time of the last successful read of this player field.", unix(time.Now()), "name", p.Name, "uuid", p.UUID, "field", f.Path))
		}
		points = append(points, gauge("mcs_minecraft_player_details_available", "All requested detail fields succeeded for this player in the last detail batch.", boolean(complete), "name", p.Name, "uuid", p.UUID), gauge("mcs_minecraft_player_details_attempt_timestamp_seconds", "Unix time of the latest player detail attempt.", unix(c.DetailsAttempt), "name", p.Name, "uuid", p.UUID))
		c.Details[p.UUID] = points
		c.Points["minecraft_player_details"] = append(c.Points["minecraft_player_details"], points...)
	}
	h.Duration = time.Since(h.LastAttempt)
	h.Success = !failed
	if failed {
		h.Errors++
	} else {
		h.LastSuccess = time.Now()
	}
	c.Health["minecraft_player_details"] = h
	end, err := processIdentity("/proc", pid)
	if err != nil || end != start || m.source.PID() != pid {
		c.Details = make(map[string][]point)
		delete(c.Points, "minecraft_player_details")
		h.Success = false
		c.Health["minecraft_player_details"] = h
	}
}

func (m *Manager) selfPoints() ([]point, error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	points := []point{
		gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine())),
		gauge("go_threads", "Number of OS threads created by the Go runtime.", float64(runtimeThreads())),
		gauge("go_info", "Go runtime information.", 1, "version", runtime.Version()),
		gauge("go_gomaxprocs", "Maximum Go threads executing user code simultaneously.", float64(runtime.GOMAXPROCS(0))),
		gauge("go_memstats_alloc_bytes", "Bytes allocated and still in use.", float64(mem.Alloc)),
		counter("go_memstats_alloc_bytes_total", "Total bytes allocated, including freed objects.", float64(mem.TotalAlloc)),
		gauge("go_memstats_sys_bytes", "Bytes obtained from the system.", float64(mem.Sys)),
		counter("go_memstats_mallocs_total", "Total heap objects allocated.", float64(mem.Mallocs)),
		counter("go_memstats_frees_total", "Total heap objects freed.", float64(mem.Frees)),
		gauge("go_memstats_heap_alloc_bytes", "Heap bytes allocated and still in use.", float64(mem.HeapAlloc)),
		gauge("go_memstats_heap_sys_bytes", "Heap bytes obtained from the system.", float64(mem.HeapSys)),
		gauge("go_memstats_heap_idle_bytes", "Heap bytes in idle spans.", float64(mem.HeapIdle)),
		gauge("go_memstats_heap_inuse_bytes", "Heap bytes in in-use spans.", float64(mem.HeapInuse)),
		gauge("go_memstats_heap_released_bytes", "Heap bytes released to the operating system.", float64(mem.HeapReleased)),
		gauge("go_memstats_heap_objects", "Currently allocated heap objects.", float64(mem.HeapObjects)),
		gauge("go_memstats_stack_inuse_bytes", "Bytes in stack spans.", float64(mem.StackInuse)),
		gauge("go_memstats_stack_sys_bytes", "Stack bytes obtained from the system.", float64(mem.StackSys)),
		gauge("go_memstats_mspan_inuse_bytes", "Bytes of allocated span metadata.", float64(mem.MSpanInuse)),
		gauge("go_memstats_mcache_inuse_bytes", "Bytes of allocated cache metadata.", float64(mem.MCacheInuse)),
		gauge("go_memstats_gc_sys_bytes", "Bytes used for GC metadata.", float64(mem.GCSys)),
		gauge("go_memstats_next_gc_bytes", "Heap target for the next GC cycle.", float64(mem.NextGC)),
		counter("go_gc_cycles_total", "Completed garbage collection cycles.", float64(mem.NumGC)),
		counter("go_gc_forced_cycles_total", "Forced garbage collection cycles.", float64(mem.NumForcedGC)),
		counter("go_gc_pause_seconds_total", "Total stop-the-world GC pause time.", float64(mem.PauseTotalNs)/1e9),
		gauge("go_gc_cpu_fraction", "Fraction of available CPU time used by GC since startup.", mem.GCCPUFraction),
	}
	if mem.LastGC > 0 {
		points = append(points, gauge("go_gc_last_timestamp_seconds", "Unix time of the last completed GC.", float64(mem.LastGC)/1e9))
	}
	proc, err := readProcess("/proc", os.Getpid(), m.clockTicks, m.bootTime)
	points = append(points, gauge("process_metrics_available", "Whether Linux process accounting was readable during the latest self collection.", boolean(err == nil)))
	if err != nil {
		return points, nil
	} // Go metrics remain available on non-Linux hosts.
	points = append(points, proc.points("process")...)
	return points, nil
}

func runtimeThreads() int { n, _ := runtime.ThreadCreateProfile(nil); return n }

func statusPoints(raw json.RawMessage) ([]point, error) {
	var s struct {
		Version struct {
			Name     *string `json:"name"`
			Protocol *int    `json:"protocol"`
		} `json:"version"`
		Players struct {
			Max *int `json:"max"`
		} `json:"players"`
		IsModded *bool `json:"isModded"`
		Forge    struct {
			Mods     []json.RawMessage `json:"mods"`
			Channels []json.RawMessage `json:"channels"`
		} `json:"forgeData"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Version.Name == nil || *s.Version.Name == "" || len(*s.Version.Name) > 256 || s.Version.Protocol == nil || *s.Version.Protocol < 0 || s.Players.Max == nil || *s.Players.Max < 0 {
		return nil, errors.New("cached native status lacks valid version or player capacity")
	}
	points := []point{gauge("mcs_minecraft_protocol_version", "Native cached status protocol version.", float64(*s.Version.Protocol)), gauge("mcs_minecraft_server_info", "Native cached status version information.", 1, "version", *s.Version.Name), gauge("mcs_minecraft_status_player_capacity", "Native cached status player capacity.", float64(*s.Players.Max))}
	if s.IsModded != nil {
		points = append(points, gauge("mcs_minecraft_modded", "Native cached status modded flag.", boolean(*s.IsModded)))
	}
	if s.Forge.Mods != nil {
		points = append(points, gauge("mcs_minecraft_advertised_mods", "Mod count advertised in cached Forge status.", float64(len(s.Forge.Mods))))
	}
	if s.Forge.Channels != nil {
		points = append(points, gauge("mcs_minecraft_advertised_channels", "Channel count advertised in cached Forge status.", float64(len(s.Forge.Channels))))
	}
	return points, nil
}

func boolean(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
func indexLabel(n int) string  { return strconv.Itoa(n) }
