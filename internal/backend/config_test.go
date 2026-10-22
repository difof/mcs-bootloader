package backend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPrivateBoundary(t *testing.T) {
	good := DefaultConfig()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.RCONAddress = "0.0.0.0:25575" },
		func(c *Config) { c.BackendAddress = "example.com:25566" },
		func(c *Config) { c.RCONAddress = c.BackendAddress },
		func(c *Config) { c.Service = "unit.service;touch bad" },
		func(c *Config) { c.Protocol = 766 },
		func(c *Config) { c.StateDirectory = "." },
	} {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("unsafe config accepted: %+v", c)
		}
	}
}

func TestConfigRejectsUnknownAndTrailing(t *testing.T) {
	for _, s := range []string{`{"idle_timeuot":"1s"}`, `{} {}`, `{"idle_timeout":3600}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestSecretPermissionsAndValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPassword(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPassword(p); err == nil {
		t.Fatal("world-readable password accepted")
	}
}
