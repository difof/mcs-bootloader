package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestCLIStatsDisplayDurationsAndPreserveJSONNumbers(t *testing.T) {
	raw := []byte("mcs_minecraft_uptime_seconds 7260\nprocess_uptime_seconds 60\nmcs_startup_estimate_seconds 300\nmcs_startup_duration_seconds 280\nmcs_startup_successes_total 2\nmcs_startup_elapsed_seconds 75\nmcs_startup_remaining_seconds 225\nmcs_startup_estimate_overdue 0\nmcs_startup_estimate_known 1\n")
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	if err := out.Stats(raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2h1m0s", "1m0s", "5m0s", "3m45s"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("human duration %q missing from %q", want, stdout.String())
		}
	}
	stdout.Reset()
	out.JSON = true
	if err := out.Stats(raw); err != nil {
		t.Fatal(err)
	}
	var groups map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if groups["minecraft"].(map[string]any)["uptime_seconds"] != float64(7260) || groups["startup"].(map[string]any)["remaining_seconds"] != float64(225) {
		t.Fatalf("JSON durations lost numeric seconds: %s", &stdout)
	}
	if groups["startup"].(map[string]any)["successful_boots"] != float64(2) {
		t.Fatalf("startup observations absent: %s", &stdout)
	}
}

func TestCLIStatsDoNotInventUnknownTiming(t *testing.T) {
	var stdout bytes.Buffer
	out := newCLIOutput(&stdout, io.Discard)
	out.JSON = true
	if err := out.Stats([]byte("mcs_startup_estimate_known 0\nmcs_startup_successes_total 0\n")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"uptime_seconds", "remaining_seconds", "last_duration_seconds", "estimate_seconds"} {
		if strings.Contains(stdout.String(), "\""+key+"\"") {
			t.Errorf("unknown timing invented %s in %s", key, &stdout)
		}
	}
}
