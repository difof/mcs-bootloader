package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/difof/mcs-bootloader/internal/backend"
)

func readConfigSnapshot(path string) (backend.Config, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return backend.Config{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return backend.Config{}, "", err
	}
	if !st.Mode().IsRegular() {
		return backend.Config{}, "", errors.New("configuration must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return backend.Config{}, "", err
	}
	if len(data) > 65536 {
		return backend.Config{}, "", errors.New("configuration exceeds 64 KiB")
	}
	var cfg backend.Config
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		// Allow an older installation to run while the migration is staged.
		cfg = backend.DefaultConfig()
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
			return cfg, "", errors.New("configuration must contain supported fields and value types")
		}
		err = cfg.Validate()
	} else {
		if st.Mode().Perm() != 0600 && st.Mode().Perm() != 0640 {
			return cfg, "", errors.New(".env must have private 0600 or 0640 permissions")
		}
		cfg, err = backend.LoadEnv(data)
	}
	if err != nil {
		return backend.Config{}, "", err
	}
	password, err := cfg.Password()
	if err != nil {
		return backend.Config{}, "", err
	}
	cfg.RCONPassword = password
	digest := sha256.Sum256(data)
	return cfg, hex.EncodeToString(digest[:]), nil
}

// A reload may name the source explicitly, but cannot silently switch the
// daemon to a second .env that a future service restart would not read.
func sameConfigSource(current, requested string) error {
	if !filepath.IsAbs(requested) {
		return errors.New("reload .env path must be absolute")
	}
	canonicalCurrent, err := filepath.EvalSymlinks(current)
	if err != nil {
		return fmt.Errorf("inspect active configuration source: %w", err)
	}
	canonicalRequested, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return fmt.Errorf("inspect requested configuration source: %w", err)
	}
	if filepath.Clean(canonicalCurrent) == filepath.Clean(canonicalRequested) {
		return nil
	}
	currentInfo, currentErr := os.Stat(current)
	requestedInfo, requestedErr := os.Stat(requested)
	if currentErr == nil && requestedErr == nil && os.SameFile(currentInfo, requestedInfo) {
		return nil
	}
	return errors.New("reload .env must be the daemon's configured source (symlink aliases are allowed); changing the source requires a gateway restart")
}
