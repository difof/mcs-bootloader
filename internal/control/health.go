package control

import (
	"errors"
	"net"
	"strings"
	"time"
)

func (c *Controller) unhealthy(state State, reason string, err error, report bool) {
	c.update(state, reason, func(s *Snapshot) {
		s.Players, s.IdleSince = nil, time.Time{}
		s.LastCheck, s.LastError = c.now(), err.Error()
		if reason == "cannot determine backend state" {
			s.BackendKnown = false
		}
	})
	if report {
		c.reportErrorWithKey(stableErrorKey(err), err.Error())
	}
}

func (c *Controller) reportError(message string) {
	c.reportErrorWithKey(message, message)
}

// stableErrorKey retains the operation, remote endpoint, wrapping context, and
// underlying cause while omitting the client's ephemeral source address. Each
// health probe uses a new connection; comparing its full net.OpError string
// would therefore turn one persistent outage into a warning on every poll.
// LastError and the first emitted warning still carry the full diagnostic.
func stableErrorKey(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	var op *net.OpError
	if !errors.As(err, &op) {
		return message
	}
	key := op.Op + " " + op.Net
	if op.Addr != nil {
		key += " " + op.Addr.String()
	}
	if cause := stableErrorKey(op.Err); cause != "" {
		key += ": " + cause
	}
	return strings.ReplaceAll(message, op.Error(), key)
}

func (c *Controller) reportErrorWithKey(key, message string) {
	if key == c.reportedError {
		return
	}
	if key == "" {
		if c.reportedError != "" {
			c.logger.Info("backend_health_recovered")
		}
	} else {
		c.logger.Warn("backend_health_error", "error", message)
	}
	c.reportedError = key
}
