//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
)

// The child invokes the actual CLI main and must be replaced by the fake Java
// executable. Returning here indicates that backend-start did not exec it.
func TestBackendStartExecHelper(t *testing.T) {
	if os.Getenv("MCGW_LAUNCH_EXEC_HELPER") != "1" {
		return
	}
	for i, argument := range os.Args {
		if argument == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(90)
		}
	}
	os.Exit(91)
}

func TestBackendStartExecUsesAppliedSocketAndPreservesServerFiles(t *testing.T) {
	// A short private directory also keeps the Unix socket below Linux's path
	// length limit, independent of the ordinary Go test-directory name.
	directory, err := os.MkdirTemp("", "mcgw-launch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	serverDirectory := filepath.Join(directory, "server with spaces")
	java := filepath.Join(directory, "fake-java")
	record := filepath.Join(directory, "launch-record")
	socket := filepath.Join(directory, "control.sock")
	pending := filepath.Join(directory, "pending.env")
	invalidEnv := []byte("MC_RCON_PASSWORD=pending-unapplied-test-password\nunfinished .env edit\n")
	if err := os.WriteFile(pending, invalidEnv, 0600); err != nil {
		t.Fatal(err)
	}
	// This is the replacement executable, not a shell used by the launcher.
	// NUL-separated output retains spaces/metacharacters and proves PID reuse.
	script := "#!/bin/sh\nset -eu\nprintf '%s\\000' \"$PWD\" \"$0\" \"$@\" \"$$\" > \"$MCGW_LAUNCH_RECORD\"\n"
	if err := os.WriteFile(java, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := backend.DefaultConfig()
	cfg.ServerDirectory, cfg.LaunchExecutable, cfg.AdminSocket = serverDirectory, filepath.Join("..", "fake-java"), socket
	cfg.RCONPassword, cfg.RCONPasswordFile = runtimeTestNextSecret, ""
	cfg.LaunchArgs = []string{"@user args $(touch must-not-exist).txt", "@libraries/loader/args ; literal.txt", "nogui", "", "${HOME}", "literal \u0027 quotes \" and 🚀"}
	beforeProperties := []byte("# retain these exact bytes\r\nonline-mode=false\r\nlevel-name=world\r\nrcon.password=" + runtimeTestSecret + "\r\nmotd=Original server\r\n")
	files := map[string][]byte{
		"server.properties":     beforeProperties,
		cfg.LaunchArgs[0][1:]:   []byte("-Xms1G\n-Xmx2G\n"),
		cfg.LaunchArgs[1][1:]:   []byte("existing.loader.Main\n--existing-argument\n"),
		"world/level.dat":       {0x0a, 0x00, 0xff, 0x12, 0x00, 0x34},
		"mods/valuable-mod.jar": []byte("disposable unchanged mod sentinel\x00"),
	}
	for name, data := range files {
		path := filepath.Join(serverDirectory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
	}
	// Select distinct disposable ports without contacting any Minecraft service.
	backendPort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendPort.Close()
	rconPort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rconPort.Close()
	cfg.BackendAddress, cfg.RCONAddress = backendPort.Addr().String(), rconPort.Addr().String()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	var requests atomic.Int32
	httpServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/backend" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(backendEnvelope{Config: cfg, Password: cfg.RCONPassword})
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- httpServer.Serve(listener) }()
	t.Cleanup(func() {
		_ = httpServer.Close()
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("launcher fixture socket: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("launcher fixture socket did not stop")
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestBackendStartExecHelper$", "--",
		"backend-start", "--socket", socket, "--config", pending)
	child.Dir = directory
	child.Env = append(os.Environ(), "MCGW_LAUNCH_EXEC_HELPER=1", "MCGW_LAUNCH_RECORD="+record)
	_ = backendPort.Close()
	_ = rconPort.Close()
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("backend-start subprocess failed: %v; output: %s", err, output)
	}
	if len(output) != 0 {
		t.Fatal("successful launcher printed unexpected output")
	}
	if requests.Load() != 1 {
		t.Fatal("launcher did not retrieve the applied configuration exactly once")
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	wantRecord := append([]string{serverDirectory, java}, cfg.LaunchArgs...)
	wantRecord = append(wantRecord, strconv.Itoa(child.Process.Pid), "")
	if !reflect.DeepEqual(strings.Split(string(recorded), "\x00"), wantRecord) {
		t.Fatal("exec changed the process ID, working directory, or literal launch arguments")
	}
	files["server.properties"] = bytes.Replace(beforeProperties, []byte("rcon.password="+runtimeTestSecret), []byte("rcon.password="+runtimeTestNextSecret), 1)
	checked := 0
	if err := filepath.WalkDir(serverDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(serverDirectory, path)
		if err != nil {
			return err
		}
		want, exists := files[name]
		if !exists {
			t.Errorf("launcher created unexpected server file: %s", name)
			return nil
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("launcher changed unexpected bytes in %s", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0640 {
			t.Errorf("launcher changed permissions for %s", name)
		}
		checked++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if checked != len(files) {
		t.Fatal("launcher removed an existing server file")
	}
	if got, err := os.ReadFile(pending); err != nil || !bytes.Equal(got, invalidEnv) {
		t.Fatal("launcher changed the pending .env")
	}
}
