package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDirectoryScanCountsRegularFilesWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "one"), filepath.Join(child, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "two"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(root, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err = sparse.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	sparse.Close()
	external := t.TempDir()
	if err = os.WriteFile(filepath.Join(external, "excluded"), make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(external, filepath.Join(root, "directory-link")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "one"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	socket, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := scanDirectory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != (1<<20)+8 || got.Files != 3 || got.Directories != 2 || got.Symlinks != 2 || got.Special != 2 {
		t.Fatalf("unexpected totals: %+v", got)
	}
	if got.Allocated >= got.Bytes {
		t.Fatalf("sparse-file allocation not reflected: %+v", got)
	}
}

func TestDirectoryScanRefusesSymlinkRootsAndHonorsCancellation(t *testing.T) {
	parent := t.TempDir()
	actual := filepath.Join(parent, "actual")
	if err := os.MkdirAll(filepath.Join(actual, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "alias")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "nested"), filepath.Join(parent, "missing")} {
		if totals, err := scanDirectory(context.Background(), path); err == nil || totals != (directoryTotals{}) {
			t.Fatalf("accepted unsafe/unavailable root %s: %+v, %v", path, totals, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanDirectory(ctx, actual); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
}

func TestDirectoryScanDepthLimitNeverReturnsPartialTotals(t *testing.T) {
	root := t.TempDir()
	path := root
	for i := 0; i < maxDirectoryDepth+1; i++ {
		path = filepath.Join(path, "d")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if totals, err := scanDirectory(context.Background(), root); err == nil || totals != (directoryTotals{}) {
		t.Fatalf("depth bound returned partial/successful totals: %+v, %v", totals, err)
	}
}

func directoryConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Directories = map[string]string{"world": t.TempDir()}
	cfg.DirectoryInterval = time.Hour
	cfg.DirectoryTimeout = time.Second
	return cfg
}

func TestDirectoryScrapesStayCachedAndNeverExposeConfiguredPaths(t *testing.T) {
	cfg := directoryConfig(t)
	path := cfg.Directories["world"]
	if err := os.WriteFile(filepath.Join(path, "region"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	m := New(Source{}, "test", testLogger())
	t.Cleanup(func() { m.Close() })
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.directories["world"].Success })
	if err := os.WriteFile(filepath.Join(path, "region"), []byte("changed saved data"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		response := httptest.NewRecorder()
		m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
		body := response.Body.String()
		if !strings.Contains(body, "mcs_directory_size_bytes{directory=\"world\"} 5\n") {
			t.Fatal("scrape rescanned the directory")
		}
		if strings.Contains(body, path) {
			t.Fatal("configured path leaked as metric label")
		}
	}
}

func TestDirectoryErrorsRetainLastGoodSizesAndExposeFailure(t *testing.T) {
	m := New(Source{}, "test", testLogger())
	t.Cleanup(func() { m.Close() })
	var attempts atomic.Int64
	m.mu.Lock()
	m.directoryScan = func(context.Context, string) (directoryTotals, error) {
		if attempts.Add(1) == 1 {
			return directoryTotals{Bytes: 42, Files: 1, Directories: 1}, nil
		}
		return directoryTotals{}, errors.New("unavailable")
	}
	m.mu.Unlock()
	cfg := directoryConfig(t)
	cfg.DirectoryInterval = time.Millisecond
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		s := m.directories["world"]
		return !s.Success && s.Errors > 0 && !s.LastSuccess.IsZero()
	})
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	for _, expected := range []string{"mcs_directory_size_bytes{directory=\"world\"} 42\n", "mcs_directory_scan_success{directory=\"world\"} 0\n", "mcs_directory_scan_last_success_timestamp_seconds{directory=\"world\"}"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("missing %s", expected)
		}
	}
}

func TestDirectoryTimeoutDoesNotDelayOtherTelemetryAndReloadPurgesRoots(t *testing.T) {
	m := New(Source{}, "test", testLogger())
	t.Cleanup(func() { m.Close() })
	entered := make(chan struct{}, 4)
	m.mu.Lock()
	m.directoryScan = func(ctx context.Context, _ string) (directoryTotals, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return directoryTotals{}, ctx.Err()
	}
	m.mu.Unlock()
	cfg := directoryConfig(t)
	cfg.DirectoryTimeout = 10 * time.Millisecond
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scan did not start")
	}
	eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.directories["world"].Errors > 0 && !m.cache.At.IsZero()
	})
	m.mu.Lock()
	sample := m.directories["world"]
	sample.LastSuccess = time.Now()
	sample.Totals.Bytes = 123
	m.directories["world"] = sample
	m.mu.Unlock()
	replacement := directoryConfig(t)
	replacement.Listen = cfg.Listen
	replacement.DirectoryTimeout = time.Hour
	// Keep a name but change its path: old successful totals must not survive.
	if err := m.Reconfigure(replacement); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	after := m.directories["world"]
	m.mu.RUnlock()
	if !after.LastSuccess.IsZero() || after.Totals.Bytes != 0 {
		t.Fatal("changed path retained old totals")
	}
	replacement.Directories = map[string]string{}
	if err := m.Reconfigure(replacement); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(response.Body.String(), "directory=\"world\"") {
		t.Fatal("removed root remained in metrics")
	}
}

func TestDirectoryBlockedFilesystemDoesNotBlockClose(t *testing.T) {
	m := New(Source{}, "test", testLogger())
	entered, release := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int64
	defer close(release)
	m.mu.Lock()
	m.directoryScan = func(context.Context, string) (directoryTotals, error) {
		if attempts.Add(1) == 1 {
			close(entered)
		}
		<-release
		return directoryTotals{}, nil
	}
	m.mu.Unlock()
	cfg := directoryConfig(t)
	if err := m.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scan did not start")
	}
	// Repeated reloads must never accumulate more workers behind a hung mount.
	for i := 0; i < 8; i++ {
		cfg.Directories = map[string]string{"other": t.TempDir()}
		if err := m.Reconfigure(cfg); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Fatalf("reload spawned %d concurrent scans", attempts.Load())
	}
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked filesystem operation")
	}
}
