package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIFlagsNeverConsumeMinecraftText(t *testing.T) {
	text := `{"text":"{{.UNCHANGED}} $(touch nope) ` + "`uname`" + `"}`
	tests := []struct {
		args, want []string
		json       bool
	}{
		{[]string{"--json", "command", "--socket", "/tmp/x", "--", "tellraw", "@a", text}, []string{"tellraw", "@a", text}, true},
		{[]string{"command", "say", "--json", "--color", "always"}, []string{"say", "--json", "--color", "always"}, false},
		{[]string{"command", "--json", "--timeout", "2s", "say", text}, []string{"say", text}, true},
	}
	for _, tc := range tests {
		out := newCLIOutput(io.Discard, io.Discard)
		got, err := parseCLI(tc.args, out)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Args, tc.want) || got.JSON != tc.json {
			t.Fatalf("got %+v, expected %q JSON=%v", got, tc.want, tc.json)
		}
	}
	for _, args := range [][]string{{"backup"}, {"command", "--timeout", "0s", "list"}, {"status", "--follow"}, {"logs", "--lines", "0"}, {"metrics", "--json", "--raw"}, {"status", "--pid", "1"}, {"remove", "--yes", "--dry-run"}} {
		if _, err := parseCLI(args, newCLIOutput(io.Discard, io.Discard)); err == nil {
			t.Fatalf("accepted invalid flags %q", args)
		}
	}
}

func TestCLINoArgumentsShowHelpAndJSONErrorsRemainStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newCLIOutput(&stdout, &stderr)
	if err := executeCLI(context.Background(), nil, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Usage: mcs-bootloader") || strings.Contains(stdout.String(), "Traceback") {
		t.Fatalf("unexpected default output: %s", &stdout)
	}
	stdout.Reset()
	out = newCLIOutput(&stdout, &stderr)
	err := executeCLI(context.Background(), []string{"--json", "--color", "always", "does-not-exist"}, out)
	if err == nil {
		t.Fatal("unknown command accepted")
	}
	out.Error(err)
	if !json.Valid(stderr.Bytes()) || strings.Contains(stderr.String(), "\x1b") {
		t.Fatalf("not clean JSON: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatal("error produced partial stdout")
	}
}

func TestCLIFormattingColorsOnlyHumanOutput(t *testing.T) {
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	out.Color = "always"
	if err := out.Result("Status", map[string]any{"state": "ready", "players": 2.0, "last_error": "untrusted\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "\x1b[1;36mStatus") || strings.Contains(stdout.String(), "untrusted\x1b") {
		t.Fatalf("unsafe or uncolored output: %q", stdout.String())
	}
	stdout.Reset()
	out.JSON = true
	if err := out.Result("Status", map[string]any{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), "\x1b") {
		t.Fatal("JSON contains formatting")
	}
	stdout.Reset()
	out.JSON = false
	out.Color = "auto"
	if err := out.Result("Status", map[string]any{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\x1b") {
		t.Fatal("piped output has ANSI")
	}
}

func TestCLITaskAdapterPreservesOriginalArguments(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "123")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/bin/task", filepath.Join(parent, "exe")); err != nil {
		t.Fatal(err)
	}
	want := []string{"tellraw", "@a", `{"text":"{{.KEEP}} $(literal)"}`, "--json", ""}
	argv := append([]string{"task", "command", "--"}, want...)
	if err := os.WriteFile(filepath.Join(parent, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := taskArguments(root, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("literal arguments changed: %q", got)
	}
	if err := os.Remove(filepath.Join(parent, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(parent, "exe")); err != nil {
		t.Fatal(err)
	}
	if _, err := taskArguments(root, 123); err == nil {
		t.Fatal("non-Task parent accepted")
	}
}

func TestCLIMetricsStructuredLabelsAndSpecialNumbers(t *testing.T) {
	raw := []byte("# HELP foo Friendly counter.\n# TYPE foo gauge\nfoo{player=\"a\\\"b\",world=\"hello\\nworld\"} 12\nfoo{player=\"none\"} NaN\n")
	samples, families, err := parseMetrics(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || len(families) != 1 || samples[0].Labels["player"] != "a\"b" || samples[0].Labels["world"] != "hello\nworld" || samples[1].Value != "NaN" {
		t.Fatalf("incorrect parse: %+v %+v", samples, families)
	}
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	out.JSON = true
	if err := out.Metrics(raw, false); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("invalid JSON %q", stdout.String())
	}
	stdout.Reset()
	out.JSON = false
	if err := out.Metrics(raw, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), raw) {
		t.Fatal("raw metrics changed")
	}
}

func TestCLIJournalRecordsRedactPasswordAndPreserveStructure(t *testing.T) {
	const password = "journal-secret-test"
	message := `{"time":"2026-10-23T12:00:00Z","level":"WARN","msg":"gateway_notice","detail":"journal-secret-test","nested":{"password":"journal-secret-test"}}`
	raw, _ := json.Marshal(map[string]any{"MESSAGE": message, "PRIORITY": "6", "__REALTIME_TIMESTAMP": "1792756800000000", "_SYSTEMD_UNIT": "mcs-bootloader.service"})
	record, err := journalRecord(raw, password)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(record)
	if bytes.Contains(encoded, []byte(password)) {
		t.Fatal("journal secret leaked")
	}
	if record["level"] != "WARN" || record["msg"] != "gateway_notice" {
		t.Fatalf("lost structured log %+v", record)
	}
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	out.JSON = true
	if err := out.Log(record); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatal("log is not NDJSON")
	}
}

func TestCLIInitWorksWithoutSourceOrTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "literal {{.ENV}}.env")
	var stdout, stderr bytes.Buffer
	out := newCLIOutput(&stdout, &stderr)
	if err := executeCLI(context.Background(), []string{"init", path, "--json"}, out); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("init output stdout=%q stderr=%q", &stdout, &stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("replace-with-a-random-private-password")) {
		t.Fatal("password marker not replaced")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private config mode %o", info.Mode().Perm())
	}
	stdout.Reset()
	if err := executeCLI(context.Background(), []string{"init", path, "--json"}, out); err == nil {
		t.Fatal("init overwrote existing config")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, current) {
		t.Fatal("refused init changed configuration")
	}
}

func TestCLISocketStatusAndLiteralCommandDoNotNeedConfig(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcs-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, map[string]any{"version": "test", "lifecycle": map[string]any{"state": "ready", "players": 2}, "traffic": map[string]any{"active_relays": 2}})
	})
	wanted := `tellraw @a {"text":"{{.KEEP}} $(literal) --json"}`
	mux.HandleFunc("POST /v1/command", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request commandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Command != wanted {
			t.Errorf("command changed: %q", request.Command)
		}
		jsonReply(w, commandResponse{Reply: "done"})
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	if err := executeCLI(context.Background(), []string{"status", "--socket", socket, "--json"}, out); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"players":2`) {
		t.Fatalf("status JSON incorrect %q", stdout.String())
	}
	stdout.Reset()
	if err := executeCLI(context.Background(), []string{"command", "--socket", socket, "--json", "--", "tellraw", "@a", `{"text":"{{.KEEP}} $(literal) --json"}`}, out); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "{\"reply\":\"done\"}\n" || calls.Load() != 1 {
		t.Fatalf("command output %q calls %d", stdout.String(), calls.Load())
	}
}

func TestCLIManagementPrivilegeForwardingPinsCanonicalEnv(t *testing.T) {
	got := withEnvArgument([]string{"--env", "relative.env", "gateway", "restart", "--json"}, "/etc/example/.env")
	want := []string{"--env", "/etc/example/.env", "gateway", "restart", "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := parseCLI([]string{"--json", "command", "--timeout=1h", "list"}, newCLIOutput(io.Discard, io.Discard)); err == nil {
		t.Fatal("excessive command timeout accepted")
	}
	if value := commandTimeout(0, map[string]string{"MC_COMMAND_TIMEOUT": "bad"}); value > 0 {
		t.Fatal("invalid configured timeout accepted")
	}
	if value := commandTimeout(time.Second, nil); value != time.Second {
		t.Fatal("explicit timeout ignored")
	}
}

func TestCLIRejectInvalidCommandBeforeConfigRead(t *testing.T) {
	out := newCLIOutput(io.Discard, io.Discard)
	err := executeCLI(context.Background(), []string{"command", "--env", "/nonexistent/config", "say", "x\nstop"}, out)
	if err == nil || errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "open .env") {
		t.Fatalf("wrong validation order %v", err)
	}
}

func TestCLIEarlyJSONSelectionRespectsLiteralValues(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--bad", "--json"}, true},
		{[]string{"command", "say", "--json"}, false},
		{[]string{"command", "--", "--json"}, false},
		{[]string{"--env", "--json", "status"}, false},
		{[]string{"command", "--json", "say", "--json=false"}, true},
	}
	for _, tc := range cases {
		if got := requestedJSON(tc.args); got != tc.want {
			t.Fatalf("%q JSON=%v want %v", tc.args, got, tc.want)
		}
	}
	var stderr bytes.Buffer
	out := newCLIOutput(io.Discard, &stderr)
	err := executeCLI(context.Background(), []string{"--bad", "--json"}, out)
	if err == nil {
		t.Fatal("invalidflagaccepted")
	}
	out.Error(err)
	if !json.Valid(stderr.Bytes()) {
		t.Fatalf("early parse error not JSON %q", stderr.String())
	}
}

func TestCLIPrivilegeForwardingPreservesLiteralOptionWords(t *testing.T) {
	original := []string{"command", "--env", "old.env", "say", "--env", "do not change", "--color=always"}
	got := withEnvArgument(original, "/etc/example/.env")
	want := []string{"--env", "/etc/example/.env", "command", "say", "--env", "do not change", "--color=always"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("privilege forwarding changed command text: %q", got)
	}
}

func TestCLIStatsHaveNamedDirectorySizesAndBoundedSummary(t *testing.T) {
	raw := []byte("# HELP mcs_directory_size_bytes Logical size.\n# TYPE mcs_directory_size_bytes gauge\nmcs_directory_size_bytes{directory=\"world\"} 1048576\nmcs_directory_scan_success{directory=\"world\"} 1\nmcs_controller_state{state=\"ready\"} 1\nmcs_jvm_perfdata_value{counter=\"noisy.details\"} 99\n")
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	if err := out.Stats(raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "world") || !strings.Contains(stdout.String(), "1 MiB") || strings.Contains(stdout.String(), "noisy.details") {
		t.Fatalf("not a useful bounded summary: %q", stdout.String())
	}
	stdout.Reset()
	out.JSON = true
	if err := out.Stats(raw); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), "1048576") {
		t.Fatalf("statsJSONlostnumericbytes: %s", &stdout)
	}
}
