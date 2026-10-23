package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

const runtimeTestSecret = "runtime-private-test-password"
const runtimeTestNextSecret = "runtime-replacement-test-password"

type runtimeRCONUpdate struct {
	password string
	timeout  time.Duration
	replace  bool
}

type runtimeCommandCall struct {
	command  string
	timeout  time.Duration
	password string
}

type runtimeTestBackend struct {
	mu              sync.Mutex
	running         bool
	players         int
	starts, stops   int
	passwordAtStart string
	password        string
	runningHook     func(context.Context) (bool, error)
	updates         []runtimeRCONUpdate
	commands        []runtimeCommandCall
	reply           string
	commandErr      error
}

func (b *runtimeTestBackend) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	b.running = true
	b.passwordAtStart = b.password
	return nil
}

func (b *runtimeTestBackend) Running(ctx context.Context) (bool, error) {
	b.mu.Lock()
	hook, running := b.runningHook, b.running
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return running, ctx.Err()
}

func (b *runtimeTestBackend) Players(ctx context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return 0, errors.New("backend is asleep")
	}
	return b.players, ctx.Err()
}

func (b *runtimeTestBackend) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stops++
	b.running = false
	return nil
}

func (b *runtimeTestBackend) Status(ctx context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"version":{"name":"1.21.1","protocol":767},"players":{"online":1,"max":5},"isModded":true}`), ctx.Err()
}

func (b *runtimeTestBackend) ReconfigureRCON(password string, timeout time.Duration, replace bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updates = append(b.updates, runtimeRCONUpdate{password, timeout, replace})
	if replace {
		b.password = password
	}
}

func (b *runtimeTestBackend) Command(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, runtimeCommandCall{command, timeout, b.password})
	return b.reply, b.commandErr
}

type runtimeLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *runtimeLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *runtimeLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type runtimeFixture struct {
	m      *runtimeConfig
	b      *runtimeTestBackend
	values map[string]string
	log    *runtimeLogBuffer
	socket string
}

func newRuntimeFixture(t *testing.T, running bool) *runtimeFixture {
	t.Helper()
	// Keep Unix socket paths well below Linux's 108-byte limit, including
	// when Go's ordinary test directory would contain a long test name.
	dir, err := os.MkdirTemp("", "mcgw-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &runtimeFixture{
		values: map[string]string{
			"MC_RCON_PASSWORD":     runtimeTestSecret,
			"MC_IDLE_TIMEOUT":      "2h",
			"MC_POLL_INTERVAL":     "1h",
			"MC_OPERATION_TIMEOUT": "250ms",
			"MC_ADMIN_SOCKET":      filepath.Join(dir, "control.sock"),
		},
		log:    &runtimeLogBuffer{},
		socket: filepath.Join(dir, "control.sock"),
	}
	f.m = &runtimeConfig{path: filepath.Join(dir, ".env")}
	f.write(t, nil)
	cfg, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	level := &slog.LevelVar{}
	logger := slog.New(slog.NewJSONHandler(f.log, &slog.HandlerOptions{Level: level}))
	f.b = &runtimeTestBackend{running: running, players: 1, password: cfg.RCONPassword}
	c := control.New(lifecycleConfig(cfg), f.b, logger)
	s := gateway.New(trafficConfig(cfg), c, logger)
	f.m = &runtimeConfig{cfg: cfg.Clone(), path: f.m.path, digest: digest, generation: 1, controller: c, gateway: s, backend: f.b, level: level, log: logger}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("runtime fixture controller did not stop")
		}
	})
	want := control.Asleep
	if running {
		want = control.Ready
	}
	runtimeEventually(t, func() bool { return c.Snapshot().State == want })
	return f
}

func (f *runtimeFixture) write(t *testing.T, changes map[string]string) {
	t.Helper()
	for key, value := range changes {
		f.values[key] = value
	}
	keys := make([]string, 0, len(f.values))
	for key := range f.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var text strings.Builder
	for _, key := range keys {
		value, err := json.Marshal(f.values[key])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&text, "%s=%s\n", key, value)
	}
	if err := os.WriteFile(f.m.path, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func runtimeEventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime condition did not become true")
}

func TestRuntimeSnapshotsOwnLaunchArguments(t *testing.T) {
	f := newRuntimeFixture(t, false)
	first, metadata := f.m.snapshot()
	want := append([]string(nil), first.LaunchArgs...)
	first.LaunchArgs[0] = "changed-outside-runtime"
	second, current := f.m.snapshot()
	if !reflect.DeepEqual(second.LaunchArgs, want) || current != metadata {
		t.Fatal("a caller changed the applied launch arguments through a snapshot")
	}
	if changed := restartFields(second, second.Clone()); len(changed) != 0 {
		t.Fatal("equal independent argument arrays require an unnecessary restart")
	}
	changed := second.Clone()
	changed.LaunchArgs[0] = "different-argument"
	if fields := restartFields(second, changed); !reflect.DeepEqual(fields, []string{"MC_LAUNCH_ARGS"}) {
		t.Fatal("changed launch arguments were not identified as restart-required")
	}
}

func TestRuntimeReloadNoopAndCommittedGeneration(t *testing.T) {
	f := newRuntimeFixture(t, true)
	before, initial := f.m.snapshot()
	result, err := f.m.reload(context.Background(), initial.SHA256)
	if err != nil || result.Applied || result.Generation != initial.Generation || len(result.Changed) != 0 {
		t.Fatalf("unchanged reload: %+v, %v", result, err)
	}
	f.b.mu.Lock()
	count := len(f.b.updates)
	f.b.mu.Unlock()
	if count != 0 {
		t.Fatal("identical reload changed the RCON session")
	}
	f.write(t, map[string]string{
		"MC_IDLE_TIMEOUT": "4h", "MC_POLL_INTERVAL": "2h",
		"MC_OPERATION_TIMEOUT": "500ms", "MC_MAX_CONNECTIONS": "8",
		"MC_SLEEPING_MOTD": "Reloaded sleeping message", "MC_LOG_LEVEL": "debug",
	})
	next, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.reload(context.Background(), initial.SHA256); err == nil {
		t.Fatal("stale prepared digest was accepted")
	}
	if current, metadata := f.m.snapshot(); !reflect.DeepEqual(current, before) || metadata != initial {
		t.Fatal("stale digest rejection changed applied state")
	}
	result, err = f.m.reload(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Generation != initial.Generation+1 || result.SHA256 != digest {
		t.Fatalf("bad committed metadata: %+v", result)
	}
	wantChanged := []string{"idle_timeout", "log_level", "max_connections", "operation_timeout", "poll_interval", "sleeping_motd"}
	if !reflect.DeepEqual(result.Changed, wantChanged) {
		t.Fatalf("changed fields: got %v want %v", result.Changed, wantChanged)
	}
	current, metadata := f.m.snapshot()
	if !reflect.DeepEqual(current, next) || metadata != result.configMetadata || f.m.level.Level() != slog.LevelDebug {
		t.Fatal("runtime metadata/configuration/log level did not commit together")
	}
	traffic := f.m.gateway.ConfigSnapshot()
	if traffic.MaxConnections != 8 || traffic.SleepingMOTD != next.SleepingMOTD {
		t.Fatalf("traffic configuration not updated: %+v", traffic)
	}
	f.b.mu.Lock()
	updates := append([]runtimeRCONUpdate(nil), f.b.updates...)
	starts, stops := f.b.starts, f.b.stops
	f.b.mu.Unlock()
	if len(updates) != 1 || updates[0].replace || updates[0].timeout != 500*time.Millisecond || updates[0].password != runtimeTestSecret || starts != 0 || stops != 0 {
		t.Fatal("reload replaced the session or changed process state")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded)+f.log.String(), runtimeTestSecret) {
		t.Fatal("reload metadata or logs exposed a credential")
	}
}

func TestRuntimeRejectedReloadPreservesEveryAppliedComponent(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]string
		mode    os.FileMode
	}{
		{"invalid duration", map[string]string{"MC_IDLE_TIMEOUT": "0s", "MC_LOG_LEVEL": "debug"}, 0600},
		{"unknown field", map[string]string{"MC_UNKNOWN": runtimeTestNextSecret, "MC_MAX_CONNECTIONS": "7"}, 0600},
		{"restart backend", map[string]string{"MC_BACKEND_ADDRESS": "127.0.0.1:25567", "MC_MAX_CONNECTIONS": "7"}, 0600},
		{"restart listen", map[string]string{"MC_LISTEN": ":25564", "MC_LOG_LEVEL": "debug"}, 0600},
		{"restart executable", map[string]string{"MC_LAUNCH_EXECUTABLE": "./start.sh", "MC_LOG_LEVEL": "debug"}, 0600},
		{"restart args", map[string]string{"MC_LAUNCH_ARGS": `["-Xmx2G","-jar","server.jar","nogui"]`, "MC_LOG_LEVEL": "debug"}, 0600},
		{"password while running", map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret, "MC_MAX_CONNECTIONS": "7"}, 0600},
		{"public permissions", map[string]string{"MC_MAX_CONNECTIONS": "7"}, 0644},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeFixture(t, true)
			before, metadata := f.m.snapshot()
			traffic := f.m.gateway.ConfigSnapshot()
			f.write(t, tc.changes)
			if err := os.Chmod(f.m.path, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := f.m.reload(context.Background(), "")
			if err == nil {
				t.Fatal("unsafe reload was accepted")
			}
			if strings.Contains(err.Error(), runtimeTestSecret) || strings.Contains(err.Error(), runtimeTestNextSecret) {
				t.Fatal("rejected update exposed a credential")
			}
			after, afterMetadata := f.m.snapshot()
			if !reflect.DeepEqual(before, after) || metadata != afterMetadata || !reflect.DeepEqual(traffic, f.m.gateway.ConfigSnapshot()) || f.m.level.Level() != slog.LevelInfo {
				t.Fatal("rejected update partially mutated applied configuration")
			}
			f.b.mu.Lock()
			updates, starts, stops := len(f.b.updates), f.b.starts, f.b.stops
			f.b.mu.Unlock()
			if updates != 0 || starts != 0 || stops != 0 {
				t.Fatal("rejected update changed RCON or process state")
			}
		})
	}
}

func TestRuntimePasswordRotationRequiresKnownStoppedBackend(t *testing.T) {
	f := newRuntimeFixture(t, false)
	before, metadata := f.m.snapshot()
	f.b.mu.Lock()
	f.b.runningHook = func(context.Context) (bool, error) { return false, errors.New("service inspection failed") }
	f.b.mu.Unlock()
	f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret})
	if _, err := f.m.reload(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Fatalf("uncertain backend state allowed password rotation: %v", err)
	}
	after, afterMetadata := f.m.snapshot()
	if !reflect.DeepEqual(before, after) || metadata != afterMetadata {
		t.Fatal("failed rotation changed applied credentials")
	}
	f.b.mu.Lock()
	updates := len(f.b.updates)
	f.b.mu.Unlock()
	if updates != 0 {
		t.Fatal("RCON changed despite uncertain backend state")
	}
}

func TestRuntimePasswordRotationCommitsBeforeQueuedWake(t *testing.T) {
	f := newRuntimeFixture(t, false)
	entered, continueCheck := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continueCheck) }) }
	defer release()
	f.b.mu.Lock()
	f.b.runningHook = func(ctx context.Context) (bool, error) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-continueCheck:
			f.b.mu.Lock()
			defer f.b.mu.Unlock()
			return f.b.running, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	f.b.mu.Unlock()
	f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret})
	reloadDone := make(chan error, 1)
	go func() { _, err := f.m.reload(context.Background(), ""); reloadDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("password rotation did not check backend state")
	}
	wakeDone := make(chan error, 1)
	go func() { wakeDone <- f.m.controller.Wake(context.Background()) }()
	f.b.mu.Lock()
	starts := f.b.starts
	f.b.mu.Unlock()
	if starts != 0 {
		t.Fatal("wake ran during password precommit check")
	}
	release()
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("password rotation did not finish")
	}
	select {
	case err := <-wakeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued wake did not finish")
	}
	cfg, metadata := f.m.snapshot()
	f.b.mu.Lock()
	updates := append([]runtimeRCONUpdate(nil), f.b.updates...)
	passwordAtStart, startCount := f.b.passwordAtStart, f.b.starts
	f.b.mu.Unlock()
	if cfg.RCONPassword != runtimeTestNextSecret || metadata.Generation != 2 || len(updates) != 1 || !updates[0].replace || passwordAtStart != runtimeTestNextSecret || startCount != 1 {
		t.Fatal("wake did not use the atomically applied replacement credential")
	}
	if strings.Contains(f.log.String(), runtimeTestNextSecret) {
		t.Fatal("password rotation logged credential value")
	}
}

func TestRuntimeCommandUsesAppliedCredentialAndRedactsRepliesAndErrors(t *testing.T) {
	f := newRuntimeFixture(t, true)
	f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret})
	f.b.mu.Lock()
	f.b.reply = "Server output includes " + runtimeTestSecret
	f.b.mu.Unlock()
	reply, err := f.m.command(context.Background(), "list", time.Second)
	if err != nil || strings.Contains(reply, runtimeTestSecret) || !strings.Contains(reply, "[REDACTED]") {
		t.Fatalf("command output was not redacted: %v", err)
	}
	f.b.mu.Lock()
	f.b.commandErr = errors.New("backend diagnostic includes " + runtimeTestSecret)
	f.b.mu.Unlock()
	if _, err := f.m.command(context.Background(), "list", time.Second); err == nil || strings.Contains(err.Error(), runtimeTestSecret) {
		t.Fatal("command error was missing or exposed the applied credential")
	}
	f.b.mu.Lock()
	calls := append([]runtimeCommandCall(nil), f.b.commands...)
	f.b.mu.Unlock()
	if len(calls) != 2 || calls[0].password != runtimeTestSecret || calls[1].password != runtimeTestSecret {
		t.Fatal("pending disk edit changed command authentication or caused a retry")
	}
	cfg, metadata := f.m.snapshot()
	if cfg.RCONPassword != runtimeTestSecret || metadata.Generation != 1 {
		t.Fatal("command silently applied pending .env edits")
	}
}

func startRuntimeSocket(t *testing.T, f *runtimeFixture) {
	t.Helper()
	listener, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.socket, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: adminHandler(f.m.controller, f.m.gateway, f.m), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("private control server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("private control server did not close")
		}
	})
}

func runtimeHTTPCall(t *testing.T, socket, method, path, body string) (int, []byte) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequest(method, "http://localhost/v1/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestRuntimePrivateSocketExposesRedactedAppliedConfig(t *testing.T) {
	f := newRuntimeFixture(t, false)
	startRuntimeSocket(t, f)
	f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret, "MC_MAX_CONNECTIONS": "7"})
	status, body := runtimeHTTPCall(t, f.socket, "GET", "config", "")
	if status != http.StatusOK || bytes.Contains(body, []byte(runtimeTestSecret)) || bytes.Contains(body, []byte(runtimeTestNextSecret)) || !bytes.Contains(body, []byte("[REDACTED]")) {
		t.Fatal("configuration endpoint did not keep credentials private")
	}
	var response struct {
		Configuration configMetadata `json:"configuration"`
		Effective     backend.Config `json:"effective"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Configuration.Generation != 1 || response.Effective.MaxConnections != 64 || response.Effective.RCONPassword != "" {
		t.Fatal("configuration endpoint exposed pending edits")
	}
	applied, err := appliedBackendConfig(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if applied.RCONPassword != runtimeTestSecret || applied.MaxConnections != 64 {
		t.Fatal("backend launcher would consume pending .env edits")
	}
	st, err := os.Stat(f.socket)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("control fixture socket is not private")
	}
	_, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(reloadRequest{ExpectedSHA256: digest})
	status, body = runtimeHTTPCall(t, f.socket, "POST", "reload", string(raw))
	if status != http.StatusOK {
		t.Fatalf("private reload rejected: %s", body)
	}
	var result reloadResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Generation != 2 || bytes.Contains(body, []byte(runtimeTestNextSecret)) {
		t.Fatal("private reload returned wrong or sensitive metadata")
	}
	applied, err = appliedBackendConfig(f.socket)
	if err != nil || applied.RCONPassword != runtimeTestNextSecret || applied.MaxConnections != 7 {
		t.Fatal("backend launcher did not receive newly applied configuration")
	}
}

func TestConfigurationSocketAllowsJSONExpansionWithinItsBound(t *testing.T) {
	f := newRuntimeFixture(t, false)
	// This valid literal is only 32 KiB in .env, but nearly 192 KiB once
	// encoding/json escapes HTML characters in the socket response.
	argument := strings.Repeat("<", 32768)
	raw, err := os.ReadFile(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("MC_LAUNCH_ARGS='[\""+argument+"\"]'\n")...)
	if err := os.WriteFile(f.m.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, digest, err := readConfigSnapshot(f.m.path)
	if err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	f.m.cfg, f.m.digest = cfg.Clone(), digest
	f.m.mu.Unlock()
	encoded, err := json.Marshal(backendEnvelope{Config: cfg, Password: cfg.RCONPassword})
	if err != nil || len(encoded) <= 65536 || len(encoded) > maxConfigResponseBytes {
		t.Fatal("fixture does not reproduce bounded JSON response expansion")
	}
	startRuntimeSocket(t, f)
	applied, err := appliedBackendConfig(f.socket)
	if err != nil || !reflect.DeepEqual(applied.LaunchArgs, []string{argument}) || applied.RCONPassword != cfg.RCONPassword {
		t.Fatalf("backend helper could not read valid expanded configuration: %v", err)
	}
	// Exercise the human-facing CLI reader as well, using a file so a large
	// response cannot fill a pipe buffer and block the test.
	output, err := os.CreateTemp(t.TempDir(), "config-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	stdout := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = stdout }()
	if err := adminClient("config", []string{"--socket", f.socket}); err != nil {
		t.Fatalf("config CLI could not read valid expanded configuration: %v", err)
	}
	body, err := os.ReadFile(output.Name())
	if err != nil || bytes.Contains(body, []byte(runtimeTestSecret)) {
		t.Fatal("expanded CLI output exposed the password or was unreadable")
	}
	var response struct {
		Effective backend.Config `json:"effective"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !reflect.DeepEqual(response.Effective.LaunchArgs, []string{argument}) {
		t.Fatal("expanded CLI output changed launch arguments")
	}
}

func TestRuntimePrivateSocketRejectsMalformedRequestsBeforeEffects(t *testing.T) {
	f := newRuntimeFixture(t, false)
	startRuntimeSocket(t, f)
	cases := []struct{ path, body string }{
		{"reload", `{}`},
		{"reload", `{"expected_sha256":"` + strings.Repeat("0", 64) + `","unknown":true}`},
		{"reload", `null`},
		{"command", `{"command":"list","timeout":"1s","unknown":true}`},
		{"command", `{"command":"list","timeout":"1s"} {}`},
		{"command", `{"command":"say hi\nstop","timeout":"1s"}`},
		{"command", `{"command":"list","timeout":"0s"}`},
		{"command", `{"command":"list","timeout":"6m"}`},
		{"command", `{"command":"list"}`},
		{"command", `{"command":"` + strings.Repeat("x", 256*1024) + `","timeout":"1s"}`},
	}
	for i, tc := range cases {
		status, body := runtimeHTTPCall(t, f.socket, "POST", tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Fatalf("malformed request %d returned %d: %s", i, status, body)
		}
	}
	f.b.mu.Lock()
	commands, updates, starts, stops := len(f.b.commands), len(f.b.updates), f.b.starts, f.b.stops
	f.b.mu.Unlock()
	_, metadata := f.m.snapshot()
	if commands != 0 || updates != 0 || starts != 0 || stops != 0 || metadata.Generation != 1 {
		t.Fatal("invalid request performed an operation")
	}
}

func TestRuntimePrivateSocketCommandPreservesLiteralTextAndNeverRetries(t *testing.T) {
	f := newRuntimeFixture(t, true)
	startRuntimeSocket(t, f)
	f.b.mu.Lock()
	f.b.reply = "Command done: " + runtimeTestSecret
	f.b.mu.Unlock()
	sentinel := filepath.Join(filepath.Dir(f.socket), "shell-must-not-run")
	literal := `{"text":"spaces and $(touch ` + sentinel + `) and ` + "`touch " + sentinel + "`" + `; remain literal"}`
	var output bytes.Buffer
	if err := runCommand([]string{"--socket", f.socket, "--timeout=1s", "--", "/tellraw", "@a", literal}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), runtimeTestSecret) || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatal("socket command output exposed credential")
	}
	f.b.mu.Lock()
	calls := append([]runtimeCommandCall(nil), f.b.commands...)
	f.b.commandErr = io.ErrUnexpectedEOF
	f.b.mu.Unlock()
	if len(calls) != 1 || calls[0].command != "tellraw @a "+literal || calls[0].timeout != time.Second {
		t.Fatal("socket command modified literal arguments or repeated delivery")
	}
	output.Reset()
	err := runCommand([]string{"--socket", f.socket, "--timeout=1s", "--", "give", "Player", "minecraft:stone", "1"}, &output)
	if err == nil || !strings.Contains(err.Error(), "may already have executed") || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("uncertain socket command was not reported: %v", err)
	}
	f.b.mu.Lock()
	callCount := len(f.b.commands)
	f.b.mu.Unlock()
	if callCount != 2 || output.Len() != 0 {
		t.Fatal("failed socket command was retried or printed success")
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("command text executed through a shell")
	}
}

func TestPrivateSocketCommandsUseOnePersistentRCONSessionWithoutReplay(t *testing.T) {
	f := newRuntimeFixture(t, true)
	commands := []string{
		`say quotes "stay literal"; $(printf untouched)`,
		"list",
		"give Player minecraft:stone 1",
	}
	var delivered atomic.Int32
	address, done := commandTestServer(t, func(conn net.Conn) error {
		auth, err := readCommandTestPacket(conn)
		if err != nil {
			return err
		}
		if auth.kind != 3 || auth.body != runtimeTestSecret {
			return errors.New("wrong applied RCON credential")
		}
		if err := writeCommandTestPacket(conn, auth.id, 2, ""); err != nil {
			return err
		}
		for i, want := range commands {
			request, err := readCommandTestPacket(conn)
			if err != nil {
				return err
			}
			delivered.Add(1)
			if request.kind != 2 || request.body != want {
				return errors.New("command changed or persistent session reauthenticated")
			}
			if i == len(commands)-1 {
				// The command reached the server, but its reply is lost. Closing
				// must surface uncertainty without replaying the operation.
				return nil
			}
			if err := writeCommandTestPacket(conn, request.id, 0, "completed "+runtimeTestSecret); err != nil {
				return err
			}
			boundary, err := readCommandTestPacket(conn)
			if err != nil {
				return err
			}
			if boundary.kind != 0 || boundary.id == request.id || boundary.body != "" {
				return errors.New("missing RCON response boundary")
			}
			if err := writeCommandTestPacket(conn, boundary.id, 0, "Unknown request 0"); err != nil {
				return err
			}
		}
		return nil
	})
	client := rcon.NewPersistent(address, runtimeTestSecret, time.Second)
	t.Cleanup(func() { _ = client.Close() })
	f.m.backend = &backend.Systemd{RCON: client}
	startRuntimeSocket(t, f)
	for i, command := range commands {
		var output bytes.Buffer
		err := runCommand([]string{"--socket", f.socket, "--timeout=1s", "--", command}, &output)
		if i < len(commands)-1 {
			if err != nil {
				t.Fatal(err)
			}
			if output.String() != "completed [REDACTED]\n" {
				t.Fatal("private command response was not redacted")
			}
		} else {
			if err == nil || !strings.Contains(err.Error(), "not retried") || output.Len() != 0 {
				t.Fatalf("lost RCON response did not surface once through socket: %v", err)
			}
		}
	}
	commandTestWait(t, done)
	if delivered.Load() != int32(len(commands)) {
		t.Fatal("RCON command was retried or dropped")
	}
}

func TestRestartCheckCannotBypassPasswordRotation(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			f := newRuntimeFixture(t, running)
			before, metadata := f.m.snapshot()
			f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestNextSecret})
			_, digest, err := readConfigSnapshot(f.m.path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.m.restartCheck(digest); err == nil || !strings.Contains(err.Error(), "config:apply") {
				t.Fatalf("restart bypassed password apply: %v", err)
			}
			after, current := f.m.snapshot()
			if !reflect.DeepEqual(before, after) || metadata != current {
				t.Fatal("restart preflight mutated runtime")
			}
			f.write(t, map[string]string{"MC_RCON_PASSWORD": runtimeTestSecret, "MC_LISTEN": ":25564"})
			_, digest, err = readConfigSnapshot(f.m.path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.m.restartCheck(digest)
			if err != nil || result["valid"] != true {
				t.Fatalf("safe restart preflight rejected: %v", err)
			}
			if _, err = f.m.restartCheck(metadata.SHA256); err == nil {
				t.Fatal("stale preflight digest accepted")
			}
		})
	}
}

func TestBackendStopReadsAppliedConfigBeforeInvalidPendingFile(t *testing.T) {
	f := newRuntimeFixture(t, true)
	startRuntimeSocket(t, f)
	if err := os.WriteFile(f.m.path, []byte("this is an unfinished .env edit"), 0600); err != nil {
		t.Fatal(err)
	}
	// PID 1 is rejected by the helper without sending a stop. Reaching that
	// validation proves pending broken .env did not mask the applied snapshot.
	err := run([]string{"backend-stop", "--socket", f.socket, "--config", f.m.path, "--pid", "1"})
	if err == nil || !strings.Contains(err.Error(), "valid --pid") {
		t.Fatalf("applied backend config was not used: %v", err)
	}
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if f.b.stops != 0 || len(f.b.commands) != 0 {
		t.Fatal("invalid PID caused a server action")
	}
}
