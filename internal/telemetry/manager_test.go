package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "server.properties"), []byte("white-list=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Enabled: true, Listen: addr, PollInterval: time.Hour, OperationTimeout: time.Second, PlayerInterval: time.Minute, ServerDirectory: dir, DirectoryInterval: 5 * time.Minute, DirectoryTimeout: 30 * time.Second}
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestMetricsDisabledAndAsleepNeverCommand(t *testing.T) {
	var calls atomic.Int64
	m := New(Source{Snapshot: func() Snapshot {
		return Snapshot{Lifecycle: control.Snapshot{State: control.Asleep, BackendKnown: true}}
	}, Command: func(context.Context, string, time.Duration) (string, error) { calls.Add(1); return "", nil }}, "test", testLogger())
	t.Cleanup(func() { _ = m.Close() })
	cfg := testConfig(t)
	cfg.Enabled = false
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled HTTP status=%d", w.Code)
	}
	cfg.Enabled = true
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { m.mu.RLock(); defer m.mu.RUnlock(); return !m.cache.At.IsZero() })
	for i := 0; i < 20; i++ {
		w = httptest.NewRecorder()
		m.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
		if !strings.Contains(w.Body.String(), "mcs_controller_state{state=\"asleep\"} 1\n") {
			t.Fatal("missing asleep state")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("asleep metrics issued %d RCON commands", calls.Load())
	}
}

func TestPreparedBindingFailureAndAbortPreserveListener(t *testing.T) {
	m := New(Source{}, "test", testLogger())
	t.Cleanup(func() { _ = m.Close() })
	cfg := testConfig(t)
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	bad := cfg
	bad.Listen = occupied.Addr().String()
	bad.PlayerDetails = true
	if p, err := m.Prepare(bad); err == nil {
		p.Abort()
		t.Fatal("accepted occupied listener")
	}
	m.mu.RLock()
	applied := m.cfg
	generation := m.generation
	m.mu.RUnlock()
	if !reflect.DeepEqual(applied, cfg) || generation != 1 {
		t.Fatal("failed bind changed applied configuration")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + cfg.Listen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	next := testConfig(t)
	prepared, err := m.Prepare(next)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Abort()
	prepared.Abort()
	probe, err := net.Listen("tcp", next.Listen)
	if err != nil {
		t.Fatal("aborted listener still bound:", err)
	}
	_ = probe.Close()
	cfg.Enabled = false
	if err = m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	probe, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal("disabled listener still bound:", err)
	}
	_ = probe.Close()
}

func TestCloseCancelsBlockedPollAndDoesNotHang(t *testing.T) {
	entered := make(chan struct{}, 1)
	m := New(Source{Snapshot: func() Snapshot {
		return Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true}}
	}, Command: func(ctx context.Context, _ string, _ time.Duration) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}, "test", testLogger())
	cfg := testConfig(t)
	cfg.OperationTimeout = time.Minute
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not start")
	}
	done := make(chan struct{})
	go func() { _ = m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel background command")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestImmediateEnableCloseDoesNotSpin(t *testing.T) {
	for i := 0; i < 25; i++ {
		m := New(Source{}, "test", testLogger())
		cfg := testConfig(t)
		if err := m.Reconfigure(cfg); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = m.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("immediate Close stuck")
		}
	}
}

func TestScrapesAreCachedAndPrivacyReloadPurgesIdentities(t *testing.T) {
	var calls atomic.Int64
	m := New(Source{Snapshot: func() Snapshot {
		return Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true}}
	}, Command: func(context.Context, string, time.Duration) (string, error) { calls.Add(1); return "", nil }}, "test", testLogger())
	t.Cleanup(func() { _ = m.Close() })
	m.cancel()
	<-m.done
	cfg := testConfig(t)
	cfg.PlayerDetails = true
	// Install cached data without starting the collector: the handler must not
	// perform a command regardless of the number of HTTP scrapes.
	m.mu.Lock()
	m.cfg = cfg
	m.cache.Points["minecraft_players"] = []point{gauge("mcs_minecraft_player_online", "test", 1, "name", "Alice", "uuid", "private-uuid")}
	m.mu.Unlock()
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if !strings.Contains(w.Body.String(), "Alice") {
			t.Fatal("fixture missing")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("scrapes issued RCON")
	}
	cfg.PlayerDetails = false
	p, err := m.Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.Commit()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "Alice") || strings.Contains(w.Body.String(), "private-uuid") {
		t.Fatal("privacy reload retained identity")
	}
}

func TestMetricsMethodAndContentType(t *testing.T) {
	m := New(Source{}, "test", testLogger())
	defer m.Close()
	m.cancel()
	<-m.done
	m.mu.Lock()
	m.cfg = Config{Enabled: true}
	m.mu.Unlock()
	for _, test := range []struct {
		method, path string
		code         int
	}{{"GET", "/metrics", 200}, {"HEAD", "/metrics", 200}, {"POST", "/metrics", 405}, {"GET", "/other", 404}} {
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.code {
			t.Errorf("%s %s status %d", test.method, test.path, w.Code)
		}
		if test.code == 200 && !strings.Contains(w.Header().Get("Content-Type"), "version=0.0.4") {
			t.Error("missing Prometheus text content type")
		}
		if test.method == "HEAD" && w.Body.Len() != 0 {
			t.Error("HEAD returned body")
		}
	}
}
