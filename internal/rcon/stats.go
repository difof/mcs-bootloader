package rcon

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DurationBounds are fixed, cumulative histogram boundaries in seconds. An
// implicit +Inf bucket always equals Requests. Labels never contain commands.
var DurationBounds = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

var operationNames = [...]string{"list", "list_uuids", "tick_query", "stop", "kick", "operator", "player_data"}

type OperationStats struct {
	Operation                                                      string
	Requests, Successes, Errors, Timeouts, Canceled, ResponseBytes uint64
	DurationSeconds                                                float64
	DurationBuckets                                                [len(DurationBounds)]uint64
}

// Stats contains only bounded aggregate telemetry, never credentials or replies.
type Stats struct {
	Operations                                                            []OperationStats
	ConnectionAttempts, Connections, ConnectionErrors, Reconnects         uint64
	AuthenticationAttempts, AuthenticationSuccesses, AuthenticationErrors uint64
	InFlight, Queued                                                      int64
	Connected                                                             bool
}

type operationCounters struct {
	mu                                                             sync.Mutex // keep each histogram observation and snapshot coherent
	requests, successes, errors, timeouts, canceled, responseBytes atomic.Uint64
	durationNanos                                                  atomic.Uint64
	buckets                                                        [len(DurationBounds)]atomic.Uint64
}

type clientCounters struct {
	operations                                                            [len(operationNames)]operationCounters
	connectionAttempts, connections, connectionErrors, reconnects         atomic.Uint64
	authenticationAttempts, authenticationSuccesses, authenticationErrors atomic.Uint64
	inFlight, queued                                                      atomic.Int64
	connected                                                             atomic.Int64
}

func operationIndex(command string) int {
	switch command {
	case "list":
		return 0
	case "list uuids":
		return 1
	case "tick query":
		return 2
	case "stop":
		return 3
	}
	if strings.HasPrefix(command, "kick ") {
		return 4
	}
	if strings.HasPrefix(command, "data get entity ") {
		return 6
	}
	return 5
}

func (c *Client) observe(command string, start time.Time, response string, err, contextErr error) {
	s := &c.stats.operations[operationIndex(command)]
	s.mu.Lock()
	defer s.mu.Unlock()
	d := time.Since(start)
	// Publish the request count last so completed observations are coherent.
	s.durationNanos.Add(uint64(d))
	for i, bound := range DurationBounds {
		if d.Seconds() <= bound {
			s.buckets[i].Add(1)
		}
	}
	if err == nil {
		s.successes.Add(1)
		s.responseBytes.Add(uint64(len(response)))
	} else {
		s.errors.Add(1)
		var timeout net.Error
		canceled := errors.Is(contextErr, context.Canceled) || errors.Is(err, context.Canceled)
		if !canceled && (errors.Is(contextErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())) {
			s.timeouts.Add(1)
		}
		if canceled {
			s.canceled.Add(1)
		}
	}
	s.requests.Add(1)
}

func (c *Client) Stats() Stats {
	s := c.stats
	out := Stats{ConnectionAttempts: s.connectionAttempts.Load(), Connections: s.connections.Load(), ConnectionErrors: s.connectionErrors.Load(), Reconnects: s.reconnects.Load(), AuthenticationAttempts: s.authenticationAttempts.Load(), AuthenticationSuccesses: s.authenticationSuccesses.Load(), AuthenticationErrors: s.authenticationErrors.Load(), Connected: s.connected.Load() > 0, InFlight: s.inFlight.Load(), Queued: s.queued.Load()}
	for i, name := range operationNames {
		v := &s.operations[i]
		v.mu.Lock()
		op := OperationStats{Operation: name, Requests: v.requests.Load(), Successes: v.successes.Load(), Errors: v.errors.Load(), Timeouts: v.timeouts.Load(), Canceled: v.canceled.Load(), ResponseBytes: v.responseBytes.Load(), DurationSeconds: float64(v.durationNanos.Load()) / float64(time.Second)}
		for j := range op.DurationBuckets {
			op.DurationBuckets[j] = v.buckets[j].Load()
		}
		out.Operations = append(out.Operations, op)
		v.mu.Unlock()
	}
	return out
}

// Replacement preserves monotonically increasing statistics across a password
// rotation. The old client must be closed before using the replacement.
func (c *Client) Replacement(password string, timeout time.Duration) *Client {
	next := NewPersistent(c.address, password, timeout)
	next.stats = c.stats
	return next
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	c.stats.connectionAttempts.Add(1)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		c.stats.connectionErrors.Add(1)
		return nil, err
	}
	if c.stats.connections.Add(1) > 1 {
		c.stats.reconnects.Add(1)
	}
	c.stats.connected.Add(1)
	return conn, nil
}
