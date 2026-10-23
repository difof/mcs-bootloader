package gateway

import (
	"context"
	"sync"
)

// BeginDrain closes login admission while leaving established relays intact.
// Minecraft sends the actual disconnect through RCON so its own compression,
// encryption and mod framing stay authoritative. Status pings remain available.
func (s *Server) BeginDrain() int {
	s.mu.Lock()
	s.draining = true
	active := s.activeRelays
	s.mu.Unlock()
	s.stopAdmissions()
	return active
}

func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining || s.closed
}

// WaitForRelays includes joins still dialing or completing their initial
// forwarding; a stalled peer cannot extend the caller's configured grace.
func (s *Server) WaitForRelays(ctx context.Context) error {
	for {
		s.mu.Lock()
		active, changed := s.activeRelays, s.relaysChanged
		s.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) acquireJoin() (release func(), acquired, draining bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || s.closed {
		return nil, false, true
	}
	controllerRelease, acquired := s.controller.AcquireJoin()
	if !acquired {
		return nil, false, false
	}
	s.activeRelays++
	var once sync.Once
	return func() {
		once.Do(func() {
			controllerRelease()
			s.mu.Lock()
			s.activeRelays--
			close(s.relaysChanged)
			s.relaysChanged = make(chan struct{})
			s.mu.Unlock()
		})
	}, true, false
}
