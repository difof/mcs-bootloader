// Package telemetry exports bounded, read-only Prometheus telemetry. Background
// polling owns all RCON and filesystem work; HTTP scrapes never contact Minecraft.
package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

type Manager struct {
	source                       Source
	version                      string
	log                          *slog.Logger
	changeMu                     sync.Mutex // held by Prepare until Commit/Abort
	mu                           sync.RWMutex
	cfg                          Config
	generation                   uint64
	endpoint                     *endpoint
	closed                       bool
	cancelPoll                   context.CancelFunc
	cancelDirectory              context.CancelFunc
	directories                  map[string]directorySample
	directoryWake, directoryDone chan struct{}
	directoryScan                func(context.Context, string) (directoryTotals, error)
	cache                        collection
	wake                         chan struct{}
	ctx                          context.Context
	cancel                       context.CancelFunc
	done                         chan struct{}
	clockTicks                   float64
	clockLoaded                  bool
	bootTime                     float64
	requests, requestErrors      atomic.Uint64
	requestNanos                 atomic.Uint64
}

// New is inert until a prepared configuration is committed. It does not open a
// listener, query Minecraft, attach to Java, or start the game server.
func New(source Source, version string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{source: source, version: version, log: logger, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), cache: newCollection(), directories: make(map[string]directorySample), directoryWake: make(chan struct{}, 1), directoryDone: make(chan struct{}), directoryScan: scanDirectory}
	go m.run()
	go m.runDirectories()
	return m
}

func (m *Manager) Close() error {
	m.changeMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.changeMu.Unlock()
		<-m.done
		return nil
	}
	m.closed = true
	m.cancel()
	if m.cancelPoll != nil {
		m.cancelPoll()
	}
	if m.cancelDirectory != nil {
		m.cancelDirectory()
	}
	ep := m.endpoint
	m.endpoint = nil
	m.mu.Unlock()
	m.changeMu.Unlock()
	if ep != nil {
		_ = ep.server.Close()
	}
	<-m.done
	if ep != nil {
		<-ep.done
	}
	return nil
}
