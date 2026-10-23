package gateway

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/difof/mcs-bootloader/internal/control"
)

func TestBootMessageCountsDownFromSameAttemptAndHandlesUnknownOrLate(t *testing.T) {
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s := control.Snapshot{State: control.Starting, BootStartedAt: start, BootEstimateSeconds: 100}
	for _, tc := range []struct {
		after time.Duration
		want  string
	}{
		{0, "About 1m40s remaining. elapsed=0s"},
		{20 * time.Second, "About 1m20s remaining. elapsed=20s"},
		{99500 * time.Millisecond, "About 1s remaining. elapsed=1m39s"},
		{100 * time.Second, "Taking longer than estimated; still starting. elapsed=1m40s"},
		{130 * time.Second, "Taking longer than estimated; still starting. elapsed=2m10s"},
	} {
		if got := bootMessage("{eta} elapsed={elapsed}", s, start.Add(tc.after)); got != tc.want {
			t.Errorf("at %s: got %q, want %q", tc.after, got, tc.want)
		}
	}
	s.BootEstimateSeconds = 0
	if got := bootMessage("{eta}", s, start); got != "Startup time is not known yet." {
		t.Fatalf("first boot fabricated estimate: %q", got)
	}
	s.BootEstimateSeconds, s.BootStartedAt = 100, time.Time{}
	if got := bootMessage("{eta}", s, start); got != "Startup time is not known yet." {
		t.Fatalf("adoption fabricated active start time: %q", got)
	}
	if got := bootMessage("elapsed={elapsed}", s, start); got != "elapsed=unknown" {
		t.Fatalf("adoption fabricated elapsed time: %q", got)
	}
	if got := bootMessage("Custom message", s, start); got != "Custom message" {
		t.Fatalf("plain custom message changed: %q", got)
	}
}

func TestBootMessageRenderingIsBoundedUTF8AndStateSpecific(t *testing.T) {
	now := time.Now()
	cfg := Config{BootMessage: "{eta}", StoppingMOTD: "stopping", UnknownMOTD: "unknown", FailedMOTD: "failed"}
	for state, want := range map[control.State]string{
		control.Starting: "Startup time is not known yet.", control.Asleep: "Startup time is not known yet.",
		control.Stopping: "stopping", control.Unknown: "unknown", control.Failed: "failed",
	} {
		if got := disconnectMessage(cfg, control.Snapshot{State: state}, now); got != want {
			t.Errorf("state %s: got %q, want %q", state, got, want)
		}
	}
	got := bootMessage(strings.Repeat("{eta}🎮", 400), control.Snapshot{}, now)
	if len(got) > 4096 || !utf8.ValidString(got) {
		t.Fatal("expanded message exceeded limit or cut a UTF-8 code point")
	}
}
