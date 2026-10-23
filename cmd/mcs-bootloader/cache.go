package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
)

func readCache(path string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2*1024*1024 {
		return nil, fmt.Errorf("status cache exceeds size limit")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("invalid status cache")
	}
	return b, nil
}

func persistCache(ctx context.Context, c *control.Controller, path string, log *slog.Logger) {
	timer := time.NewTicker(10 * time.Second)
	defer timer.Stop()
	caches := []persistedCache{
		{name: "status_cache", path: path, read: func() []byte { return c.CachedStatus() }},
		{name: "boot_history", path: filepath.Join(filepath.Dir(path), "boot-history.json"), read: c.CachedBootHistory},
	}

	for {
		select {
		case <-ctx.Done():
			for i := range caches {
				caches[i].persist(log)
			}
			return
		case <-timer.C:
			for i := range caches {
				caches[i].persist(log)
			}
		}
	}
}

func readBootCache(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("boot history must be a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, errors.New("boot history exceeds size limit")
	}
	return data, nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cache-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type persistedCache struct {
	name, path string
	read       func() []byte
	last       []byte
	lastError  string
}

func (entry *persistedCache) persist(log *slog.Logger) {
	data := entry.read()
	if len(data) == 0 || bytes.Equal(entry.last, data) {
		return
	}
	if err := atomicWrite(entry.path, data); err != nil {
		if err.Error() != entry.lastError {
			log.Warn(entry.name+"_write_failed", "error", err.Error())
			entry.lastError = err.Error()
		}
		return
	}
	if entry.lastError != "" {
		log.Info(entry.name + "_recovered")
		entry.lastError = ""
	}
	entry.last = append(entry.last[:0], data...)
}

func restoreCaches(c *control.Controller, path string, log *slog.Logger) {
	if cached, err := readCache(path); err == nil {
		c.SetCachedStatus(cached)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("status_cache_unavailable", "error", err.Error())
	}
	if history, err := readBootCache(filepath.Join(filepath.Dir(path), "boot-history.json")); err == nil {
		if err := c.RestoreBootHistory(history); err != nil {
			log.Warn("boot_history_unavailable", "error", err.Error())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("boot_history_unavailable", "error", err.Error())
	}
}
