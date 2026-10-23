package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestConfigPrivateBoundary(t *testing.T) {
	good := DefaultConfig()
	if good.IdleTimeout != Duration(15*time.Minute) {
		t.Fatal("default idle shutdown must be 15 minutes")
	}
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

func TestExampleMatchesRuntimeDefaults(t *testing.T) {
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnv(data)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	// A new .env uses the inline placeholder; the legacy JSON-only password
	// file is intentionally absent from all operator configuration sources.
	want.RCONPasswordFile = ""
	want.RCONPassword = "replace-with-a-random-private-password"
	if !reflect.DeepEqual(got, want) {
		t.Fatal(".env.example and Go runtime defaults differ")
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
