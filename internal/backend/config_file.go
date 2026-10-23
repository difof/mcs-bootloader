package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return c, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return c, fmt.Errorf("inspect configuration file: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return c, fmt.Errorf("configuration file must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return c, err
	}
	if len(raw) > 65536 {
		return c, fmt.Errorf("configuration exceeds 64 KiB")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		if stat.Mode().Perm() != 0600 && stat.Mode().Perm() != 0640 {
			return c, fmt.Errorf(".env must have private 0600 or 0640 permissions")
		}
		return LoadEnv(raw)
	}
	// JSON is accepted only to migrate older installations. The file name is
	// deliberately irrelevant: reload validates temporary .env snapshots.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration must be a JSON object with supported fields and value types")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration has trailing data")
	}
	return c, c.Validate()
}

// Password preserves inline credentials exactly. ReadPassword is retained only
// for older JSON configurations while an installation is being migrated.
func (c Config) Password() (string, error) {
	if c.RCONPassword != "" {
		if err := validatePassword(c.RCONPassword); err != nil {
			return "", err
		}
		return c.RCONPassword, nil
	}
	if c.RCONPasswordFile == "" {
		return "", fmt.Errorf("MC_RCON_PASSWORD is required")
	}
	return ReadPassword(c.RCONPasswordFile)
}

func validatePassword(password string) error {
	if len(password) < 16 || len(password) > 4096 || strings.ContainsAny(password, "\x00\r\n") {
		return fmt.Errorf("RCON password must be 16-4096 bytes without line breaks or NUL")
	}
	return nil
}

func ReadPassword(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open RCON password file: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0027 != 0 {
		return "", fmt.Errorf("RCON password file must be regular, not group-writable or accessible to other users (0600/0640)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(data))
	if len(password) < 16 || len(data) > 4096 || strings.ContainsAny(password, "\x00\r\n") {
		return "", fmt.Errorf("RCON password must be 16-4096 bytes without line breaks or NUL")
	}
	return password, nil
}
