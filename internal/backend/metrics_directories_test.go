package backend

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMetricsDirectoryConfigIsLiteralBoundedAndCloned(t *testing.T) {
	cfg, err := LoadEnv([]byte("MC_RCON_PASSWORD=synthetic-password-only\nMC_METRICS_DIRECTORIES='{\"world\":\"/srv/world\",\"backups\":\"/backup/$literal\"}'\nMC_METRICS_DIRECTORY_INTERVAL=2m\nMC_METRICS_DIRECTORY_TIMEOUT=10s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsDirectories["backups"] != "/backup/$literal" || cfg.MetricsDirectoryInterval != Duration(2*time.Minute) || cfg.MetricsDirectoryTimeout != Duration(10*time.Second) {
		t.Fatal("directory settings not parsed")
	}
	clone := cfg.Clone()
	clone.MetricsDirectories["world"] = "/other"
	if cfg.MetricsDirectories["world"] != "/srv/world" {
		t.Fatal("clone aliases original directory map")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"metrics_directories"`) {
		t.Fatal("public configuration omitted directory settings")
	}
}

func TestMetricsDirectoryConfigRejectsInvalidAndDuplicateEntries(t *testing.T) {
	invalid := []string{`null`, `[]`, `{"world":null}`, `{"world":3}`, `{"world":"relative"}`, `{"world":"/"}`, `{"world":"//"}`, `{"world":"/a/.."}`, `{"bad name":"/srv/world"}`, `{"world":"/a","world":"/b"}`, `{"world":"/a\nsecret"}`, `{"world":"/a\ud800"}`}
	many := make(map[string]string)
	for i := 0; i < 33; i++ {
		many[fmt.Sprintf("root%d", i)] = "/srv/world"
	}
	raw, _ := json.Marshal(many)
	invalid = append(invalid, string(raw))
	raw, _ = json.Marshal(map[string]string{strings.Repeat("A", 65): "/srv/world"})
	invalid = append(invalid, string(raw))
	raw, _ = json.Marshal(map[string]string{"world": "/" + strings.Repeat("a", 4096)})
	invalid = append(invalid, string(raw))
	for _, value := range invalid {
		t.Run(value[:min(40, len(value))], func(t *testing.T) {
			if _, err := LoadEnv([]byte("MC_RCON_PASSWORD=synthetic-password-only\nMC_METRICS_DIRECTORIES='" + value + "'\n")); err == nil {
				t.Fatal("accepted invalid directory configuration")
			}
		})
	}
	for _, key := range []string{"MC_METRICS_DIRECTORY_INTERVAL", "MC_METRICS_DIRECTORY_TIMEOUT"} {
		if _, err := LoadEnv([]byte("MC_RCON_PASSWORD=synthetic-password-only\n" + key + "=0s\n")); err == nil {
			t.Fatalf("accepted zero %s", key)
		}
	}
}
