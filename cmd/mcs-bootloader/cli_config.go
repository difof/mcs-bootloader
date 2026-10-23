package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/difof/mcs-bootloader/internal/backend"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
)

var serviceAccountPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

func resolveEnv(explicit string, create bool) (string, error) {
	path := explicit
	if path == "" {
		path = ".env"
		if !create {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				path = defaultConfigPath
			} else if err != nil {
				return "", err
			}
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if create {
		return absolute, nil
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("open .env: %w; use mcs-bootloader init to create one, or --env PATH", err)
	}
	return resolved, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func readOperatorValues(path string) (map[string]string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0640) {
		return nil, errors.New(".env must be a regular file with private 0600 or 0640 permissions")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	return backend.OperatorValues(raw)
}

func runConfigCheck(ctx context.Context, o cliOptions, out *cliOutput) error {
	path, err := resolveEnv(o.Env, false)
	if err != nil {
		return err
	}
	cfg, err := backend.LoadConfig(path)
	if err != nil {
		return err
	}
	if _, err = cfg.Password(); err != nil {
		return err
	}
	result := map[string]any{"valid": true, "path": path, "message": "Configuration and password permissions are valid."}
	if o.Command == "config check" {
		account, err := probeServiceConfigReadability(ctx, path)
		if err != nil {
			return err
		}
		result["service_account"] = account
		result["service_account_readable"] = true
	}
	return out.Result("Configuration", result)
}

func probeServiceConfigReadability(ctx context.Context, path string) (string, error) {
	raw, err := readBoundedFile(path, 65536)
	if err != nil {
		return "", err
	}
	values, err := backend.EnvValues(raw)
	if err != nil {
		return "", err
	}
	account := values["MC_SERVICE_USER"]
	if account == "" {
		account = "minecraft"
	}
	if !serviceAccountPattern.MatchString(account) || account == "root" {
		return "", errors.New("MC_SERVICE_USER must be an existing non-root account")
	}
	probe := exec.CommandContext(ctx, "sudo", "-n", "-u", account, "--", "/usr/bin/test", "-r", path)
	if err := probe.Run(); err != nil {
		return "", fmt.Errorf("configuration is not readable as service account %s; check its account, directory traversal, file permissions and sudo access", account)
	}
	return account, nil
}
