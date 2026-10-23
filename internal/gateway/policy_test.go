package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/mcproto"
)

func writeWhitelistFixture(t *testing.T, root, properties, list string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte(properties), 0600); err != nil {
		t.Fatal(err)
	}
	if list != "" {
		if err := os.WriteFile(filepath.Join(root, "whitelist.json"), []byte(list), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func loginDisconnect(t *testing.T, address, name string) []byte {
	t.Helper()
	client := dialClient(t, address)
	defer client.Close()
	if err := writeAll(client, append(handshakePacket("friends.example\x00FML3\x00", 2), loginPacket(name)...)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := mcproto.ReadPacket(client, 65536)
	if err != nil || len(payload) == 0 || payload[0] != 0 {
		t.Fatalf("expected native pre-login disconnect: %q, %v", payload, err)
	}
	return payload
}

func TestWhitelistRejectsBeforeWakeOrReadyAdmission(t *testing.T) {
	for _, state := range []control.State{control.Asleep, control.Ready} {
		t.Run(string(state), func(t *testing.T) {
			root := t.TempDir()
			writeWhitelistFixture(t, root, "white-list=true\n", `[{"uuid":"00000000-0000-0000-0000-000000000001","name":"Allowed"}]`)
			controller := &fakeController{state: state, healthy: true}
			server, address, _ := startGateway(t, Config{ServerDirectory: root, WhitelistMessage: "Private friends server"}, controller)
			payload := loginDisconnect(t, address, "Stranger")
			if !bytes.Contains(payload, []byte("Private friends server")) {
				t.Fatalf("wrong denial reason: %q", payload)
			}
			stats := server.Stats()
			if controller.wakeCount() != 0 || controller.Snapshot().ActiveConnections != 0 || stats.Routed != 0 || stats.BackendErrors != 0 || stats.WhitelistDenied != 1 {
				t.Fatalf("denied player reached backend admission: %+v", stats)
			}
		})
	}
}

func TestWhitelistRefreshesNativeFilesImmediatelyAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeWhitelistFixture(t, root, "white-list=true\n", `[]`)
	controller := &fakeController{state: control.Asleep}
	server, address, _ := startGateway(t, Config{ServerDirectory: root, BootMessage: "Booting for a friend", FailedMOTD: "Whitelist temporarily unavailable"}, controller)
	_ = loginDisconnect(t, address, "Allowed")
	if controller.wakeCount() != 0 {
		t.Fatal("empty whitelist woke the backend")
	}
	writeWhitelistFixture(t, root, "white-list=true\n", `[{"uuid":"00000000-0000-0000-0000-000000000001","name":"Allowed"}]`)
	if payload := loginDisconnect(t, address, "Allowed"); !bytes.Contains(payload, []byte("Booting for a friend")) || controller.wakeCount() != 1 {
		t.Fatal("native whitelist update was not visible on the next login")
	}
	writeWhitelistFixture(t, root, "white-list=true\n", `{not valid`)
	if payload := loginDisconnect(t, address, "Another"); !bytes.Contains(payload, []byte("Whitelist temporarily unavailable")) || controller.wakeCount() != 1 || server.Stats().WhitelistErrors != 1 {
		t.Fatal("unreadable whitelist did not fail closed")
	}
	writeWhitelistFixture(t, root, "white-list=false\n", `{still not valid`)
	if payload := loginDisconnect(t, address, "Another"); !bytes.Contains(payload, []byte("Booting for a friend")) || server.Stats().WhitelistErrors != 1 {
		t.Fatal("disabled whitelist still read/rejected whitelist.json")
	}
}

func TestLiveMOTDOverridePreservesModFieldsAndExactPing(t *testing.T) {
	cache := json.RawMessage(`{"version":{"name":"modded","protocol":767,"extra":"preserved"},"players":{"max":9,"online":2,"sample":[{"name":"Friend"}],"modFlag":[1,2]},"description":{"text":"Native MOTD","extra":[{"text":"native detail"}]},"isModded":true,"neoforgeData":{"mods":[{"id":"mod","value":1234567890123456789}]},"unknown":[0,255]}`)
	controller := &fakeController{state: control.Ready, healthy: true, players: 2, cache: cache}
	server, address, _ := startGateway(t, Config{}, controller)
	query := func() map[string]json.RawMessage {
		t.Helper()
		client := dialClient(t, address)
		defer client.Close()
		if err := writeAll(client, append(handshakePacket("friends.example", 1), mcproto.EncodePacket([]byte{0})...)); err != nil {
			t.Fatal(err)
		}
		_, payload, err := mcproto.ReadPacket(client, mcproto.MaxStatus+6)
		if err != nil {
			t.Fatal(err)
		}
		start := bytes.IndexByte(payload, '{')
		var obj map[string]json.RawMessage
		if start < 0 || json.Unmarshal(payload[start:], &obj) != nil {
			t.Fatalf("invalid status packet: %q", payload)
		}
		// Preserve a legal noncanonical VarInt and all 64 timestamp bits.
		ping := []byte{0x89, 0x00, 1, 0xff, 0x80, 0, 0x7f, 5, 0, 0xaa, 0x55}
		if err := writeAll(client, ping); err != nil {
			t.Fatal(err)
		}
		pong, _, err := mcproto.ReadPacket(client, 16)
		if err != nil || !bytes.Equal(ping, pong) {
			t.Fatalf("ping bytes changed: %x, %v", pong, err)
		}
		return obj
	}
	before := query()
	updated := server.ConfigSnapshot()
	updated.OverrideMOTD, updated.MOTD = true, "Our modpack\nJoin us"
	if err := server.Reconfigure(updated); err != nil {
		t.Fatal(err)
	}
	after := query()
	if string(after["description"]) != `{"text":"Our modpack\nJoin us"}` {
		t.Fatalf("ready MOTD not overridden: %s", after["description"])
	}
	for key, value := range before {
		if key != "description" && !bytes.Equal(value, after[key]) {
			t.Fatalf("override changed %s: %s", key, after[key])
		}
	}
	controller.mu.Lock()
	controller.state = control.Asleep
	controller.mu.Unlock()
	if got := query(); !bytes.Equal(got["description"], after["description"]) {
		t.Fatal("complete override did not apply while asleep")
	}
	updated.OverrideMOTD = false
	if err := server.Reconfigure(updated); err != nil {
		t.Fatal(err)
	}
	if got := query(); bytes.Equal(got["description"], after["description"]) {
		t.Fatal("disabled override remained active while asleep")
	}
	controller.mu.Lock()
	controller.state = control.Ready
	controller.mu.Unlock()
	if got := query(); !bytes.Equal(got["description"], before["description"]) {
		t.Fatal("disabling override did not restore the complete native component")
	}
	if controller.wakeCount() != 0 || !bytes.Equal(controller.CachedStatus(), cache) {
		t.Fatal("MOTD changes woke Minecraft or mutated its cached metadata")
	}
}

func TestDrainKeepsOpaqueRelayAndRejectsNewLogins(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	preface := append(handshakePacket("friends.example\x00FML3\x00", 2), loginPacket("Allowed")...)
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.CopyN(io.Discard, conn, int64(len(preface)))
		_, _ = io.Copy(conn, conn)
	}()
	controller := &fakeController{state: control.Ready, healthy: true}
	server, address, _ := startGateway(t, Config{BackendAddress: backend.Addr().String(), RestartMessage: "Front door restarting; reconnect soon"}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, preface); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return server.Stats().Routed == 1 })
	if count := server.BeginDrain(); count != 1 {
		t.Fatalf("unexpected relay count: %d", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := server.WaitForRelays(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain did not obey grace timeout: %v", err)
	}
	cancel()
	payload := bytes.Repeat([]byte{0x80, 0xff, 0, 0xfe}, 1024)
	if err := writeAll(client, payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("draining corrupted/closed existing opaque relay: %v", err)
	}
	if disconnect := loginDisconnect(t, address, "NewFriend"); !bytes.Contains(disconnect, []byte("Front door restarting; reconnect soon")) {
		t.Fatalf("draining login got wrong reason: %q", disconnect)
	}
	if controller.wakeCount() != 0 || server.Stats().Routed != 1 || server.Stats().DrainingRejected != 1 {
		t.Fatal("draining admitted or woke a new player")
	}
	client.Close()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.WaitForRelays(ctx); err != nil {
		t.Fatal(err)
	}
	<-backendDone
	stats := server.Stats()
	if stats.BytesToClient != uint64(len(payload)) || stats.BytesToBackend != uint64(len(preface)+len(payload)) {
		t.Fatalf("traffic byte accounting changed during drain: %+v", stats)
	}
}
