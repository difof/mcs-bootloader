package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/difof/mcs-bootloader/internal/control"
)

func TestBootCacheWorkerFlushesPrivateCompletedHistory(t *testing.T) {
	directory := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := control.New(control.Config{}, nil, log)
	history := []byte(`{"version":1,"estimate_seconds":60,"last_duration_seconds":70,"successful_boots":4}`)
	if err := c.RestoreBootHistory(history); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	persistCache(ctx, c, filepath.Join(directory, "status.json"), log)
	path := filepath.Join(directory, "boot-history.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache should be private: info=%v err=%v", info, err)
	}
	data, err := readBootCache(path)
	if err != nil || string(data) != string(history) {
		t.Fatalf("completed boot cache did not survive flush: %q %v", data, err)
	}
}

func TestBootCacheRejectsSymlinksSpecialFilesAndOversizedInput(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(directory, "public")
	if err := os.WriteFile(public, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// File creation honors the host umask; make this deliberately unsafe
	// fixture public even on an operator account configured with umask 077.
	if err := os.Chmod(public, 0644); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(directory, "oversized")
	if err := os.WriteFile(oversized, make([]byte, 4097), 0600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{directory, link, public, oversized, fifo} {
		if _, err := readBootCache(path); err == nil {
			t.Errorf("accepted unsafe cache: %s", path)
		}
	}
	if err := atomicWrite(link, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Fatalf("atomic cache write followed link: %q %v", data, err)
	}
}
