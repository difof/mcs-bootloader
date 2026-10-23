package telemetry

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

const aliceUUID = "01234567-89ab-cdef-0123-456789abcdef"

func TestNativeListUUIDAndTickFixtures(t *testing.T) {
	for _, text := range []string{"There are 0 of a max of 5 players online: ", "There are 1 of a max of 5 players online: Alice (" + aliceUUID + ")"} {
		points, err := playerPoints(text, true)
		if err != nil {
			t.Fatal(err)
		}
		body := string(encode(points))
		if !strings.Contains(body, "mcs_minecraft_players_capacity 5\n") {
			t.Fatal(body)
		}
	}
	for _, text := range []string{
		"There are 0 of a max of 5 players online: Alice",
		"There are 2 of a max of 5 players online: Alice (" + aliceUUID + ")",
		"There are 1 of a max of 5 players online: @a (" + aliceUUID + ")",
		"There are 1 of a max of 5 players online: Alice;stop (" + aliceUUID + ")",
		"There are 1 of a max of 5 players online: Alice (broken)",
	} {
		if _, err := playerPoints(text, true); err == nil {
			t.Errorf("accepted ambiguous player response %q", text)
		}
	}
	fixture := "The game is running normally\nTarget tick rate: 20.0 per second.\nAverage time per tick: 6.7ms (Target: 50.0ms)\nPercentiles: P50: 6.1ms P95: 11.3ms P99: 13.1ms, sample: 100"
	points, err := tickPoints(fixture)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encode(points))
	for _, want := range []string{"mcs_minecraft_tick_target_rate_hertz 20\n", "mcs_minecraft_tick_duration_seconds 0.0067\n", "mcs_minecraft_tick_samples 100\n", "mcs_minecraft_tick_duration_quantile_seconds{quantile=\"0.99\"}"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from %s", want, body)
		}
	}
	if _, err = tickPoints("Unknown or incomplete command"); err == nil {
		t.Fatal("unsupported tick command became a zero value")
	}
}

func TestPlayerFieldParsesOnlyRequestedNativeValues(t *testing.T) {
	p := player{Name: "Alice", UUID: aliceUUID}
	values := []string{"20.0f", "20", "5.0f", "12", "240", "0", "[12.5d, 65.0d, -31.25d]", "\"minecraft:overworld\""}
	for i, value := range values {
		points, err := playerFieldPoints(p, i, "Alice has the following entity data: "+value)
		if err != nil {
			t.Fatal(playerFields[i].Path, err)
		}
		if len(points) == 0 {
			t.Fatal("empty detail")
		}
	}
	for _, value := range []string{"NaNf", "+Infd", "20.0f trailing", "{Health:20.0f,Inventory:[secret]}", "no entity found"} {
		if _, err := playerFieldPoints(p, 0, "Alice has the following entity data: "+value); err == nil {
			t.Errorf("accepted unsafe/ambiguous detail %q", value)
		}
	}
	if _, err := playerFieldPoints(p, 0, "Bob has the following entity data: 20.0f"); err == nil {
		t.Fatal("accepted another player's data")
	}
	if _, err := playerFieldPoints(p, 6, "Alice has the following entity data: [1.0d,2.0d]"); err == nil {
		t.Fatal("accepted truncated position")
	}
	if _, err := playerFieldPoints(p, 7, "Alice has the following entity data: \"secret argument text\""); err == nil {
		t.Fatal("accepted non-resource dimension")
	}
}

func TestPlayerDetailBudgetCachingDisconnectAndPrivacy(t *testing.T) {
	var calls atomic.Int64
	snapshot := func() Snapshot {
		return Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true}}
	}
	values := map[string]string{"Health": "20.0f", "foodLevel": "20", "foodSaturationLevel": "5.0f", "XpLevel": "12", "XpTotal": "240", "playerGameType": "0", "Pos": "[12.5d, 65.0d, -31.25d]", "Dimension": "\"minecraft:overworld\""}
	m := New(Source{Snapshot: snapshot, PID: os.Getpid, Command: func(_ context.Context, command string, _ time.Duration) (string, error) {
		calls.Add(1)
		parts := strings.Fields(command)
		if len(parts) != 5 || parts[0] != "data" || parts[1] != "get" || parts[2] != "entity" || parts[3] != "Alice" {
			t.Errorf("unexpected command %q", command)
		}
		return "Alice has the following entity data: " + values[parts[4]], nil
	}}, "test", testLogger())
	m.cancel()
	<-m.done
	defer m.Close()
	cfg := testConfig(t)
	cfg.PlayerDetails = true
	players := []player{{Name: "Alice", UUID: aliceUUID}}
	c := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &c, newCollection(), players)
	if calls.Load() != 8 || !c.Health["minecraft_player_details"].Success {
		t.Fatalf("calls=%d health=%+v", calls.Load(), c.Health)
	}
	next := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &next, c, players)
	if calls.Load() != 8 || len(next.Details[aliceUUID]) == 0 {
		t.Fatal("detail interval did not reuse cache")
	}
	removed := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &removed, next, nil)
	if len(removed.Details) != 0 || len(removed.Points["minecraft_player_details"]) != 0 {
		t.Fatal("disconnected player retained detail labels")
	}
	next.removeDetails()
	if len(next.Details) != 0 || strings.Contains(string(encode(next.Points["minecraft_player_details"])), "Alice") {
		t.Fatal("privacy purge retained identity")
	}
	// A whole detail batch shares one deadline, not eight separate timeouts.
	m.source.Command = func(ctx context.Context, _ string, _ time.Duration) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	cfg.OperationTimeout = 20 * time.Millisecond
	budgeted := newCollection()
	started := time.Now()
	m.collectPlayerDetails(context.Background(), cfg, &budgeted, newCollection(), players)
	if time.Since(started) > time.Second {
		t.Fatal("detail batch exceeded its overall budget")
	}
	if budgeted.Health["minecraft_player_details"].Success {
		t.Fatal("timed out details reported success")
	}
	if strings.Contains(string(encode(budgeted.Points["minecraft_player_details"])), "# TYPE mcs_minecraft_player_health ") {
		t.Fatal("missing health invented as zero")
	}
}

func TestFormatterEscapesLabelsAndGroupsFamilies(t *testing.T) {
	points := []point{gauge("test_metric", "help\\slash\nline", 1, "name", "Alice\n\"quoted\"\\path"), gauge("test_metric", "help\\slash\nline", 2, "name", "Bob")}
	text := string(encode(points))
	if strings.Count(text, "# HELP test_metric ") != 1 || strings.Count(text, "# TYPE test_metric gauge\n") != 1 {
		t.Fatal(text)
	}
	if !strings.Contains(text, "name=\"Alice\\n\\\"quoted\\\"\\\\path\"") {
		t.Fatal("invalid escaped labels:", text)
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatal("missing final newline")
	}
}

func TestStatusMissingFieldsNeverBecomeInventedZeroes(t *testing.T) {
	for _, raw := range []string{`{}`, `{"version":{"name":"1.21.1","protocol":767}}`, `{"version":{"protocol":767},"players":{"max":5}}`, `{"version":{"name":"1.21.1","protocol":767},"players":{"max":-1}}`} {
		if points, err := statusPoints([]byte(raw)); err == nil || len(points) != 0 {
			t.Fatalf("accepted incomplete native status %s", raw)
		}
	}
	points, err := statusPoints([]byte(`{"version":{"name":"1.21.1","protocol":767},"players":{"max":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(points)), "mcs_minecraft_modded") {
		t.Fatal("absent optional modded flag became false")
	}
	points, err = statusPoints([]byte(`{"version":{"name":"1.21.1","protocol":767},"players":{"max":5},"isModded":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encode(points)), "mcs_minecraft_modded 0\n") {
		t.Fatal("explicit false modded flag omitted")
	}
}

func TestPlayerDetailBudgetRotatesPlayersAndFields(t *testing.T) {
	var commands []string
	m := New(Source{Snapshot: func() Snapshot {
		return Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true}}
	}, PID: os.Getpid, Command: func(ctx context.Context, command string, _ time.Duration) (string, error) {
		commands = append(commands, command)
		<-ctx.Done()
		return "", ctx.Err()
	}}, "test", testLogger())
	m.cancel()
	<-m.done
	defer m.Close()
	cfg := testConfig(t)
	cfg.PlayerDetails = true
	cfg.PlayerInterval = time.Nanosecond
	cfg.OperationTimeout = 5 * time.Millisecond
	players := []player{{Name: "Alice", UUID: aliceUUID}, {Name: "Bob", UUID: "11234567-89ab-cdef-0123-456789abcdef"}}
	first := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &first, newCollection(), players)
	second := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &second, first, players)
	third := newCollection()
	m.collectPlayerDetails(context.Background(), cfg, &third, second, players)
	if len(commands) != 3 || commands[0] != "data get entity Alice Health" || commands[1] != "data get entity Bob Health" || commands[2] != "data get entity Alice foodLevel" {
		t.Fatalf("detail budget did not rotate fairly: %v", commands)
	}
}

func TestUnavailableJVMStillExpectedWhileBackendRunning(t *testing.T) {
	m := New(Source{PID: func() int { return 123 }}, "test", testLogger())
	m.cancel()
	<-m.done
	defer m.Close()
	cache := newCollection()
	cache.Health["jvm_process"] = health{Expected: true, Errors: 1}
	text := string(encode(m.points(Config{}, cache, Snapshot{Lifecycle: control.Snapshot{State: control.Ready, BackendKnown: true, BackendRunning: true}}, rcon.Stats{})))
	if !strings.Contains(text, "mcs_metrics_collector_expected{collector=\"jvm_process\"} 1\n") || !strings.Contains(text, "mcs_metrics_collector_success{collector=\"jvm_process\"} 0\n") {
		t.Fatal("unreadable JVM process was incorrectly treated as not expected")
	}
}
