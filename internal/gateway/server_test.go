package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/mcproto"
)

type fakeController struct {
	mu      sync.Mutex
	state   control.State
	players int
	active  int
	wakes   int
	healthy bool
	cache   json.RawMessage
}

func (f *fakeController) Wake(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakes++
	if f.state == control.Stopping {
		return errors.New("stopping")
	}
	if f.state == control.Asleep || f.state == control.Failed {
		f.state = control.Starting
	}
	return nil
}

func (f *fakeController) Snapshot() control.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.players
	return control.Snapshot{State: f.state, Players: &p, ActiveConnections: f.active}
}

func (f *fakeController) AcquireJoin() (func(), bool) {
	f.mu.Lock()
	if f.state != control.Ready || !f.healthy {
		f.mu.Unlock()
		return nil, false
	}
	f.active++
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { f.mu.Lock(); f.active--; f.mu.Unlock() }) }, true
}

func (f *fakeController) CachedStatus() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(json.RawMessage(nil), f.cache...)
}

func (f *fakeController) wakeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.wakes }

func startGateway(t *testing.T, cfg Config, controller *fakeController) (*Server, string, context.CancelFunc) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackendAddress == "" {
		cfg.BackendAddress = "127.0.0.1:1"
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, controller, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gateway did not stop after context cancellation")
		}
	})
	return s, listener.Addr().String(), cancel
}

func handshakePacket(host string, state int) []byte {
	payload := mcproto.AppendVarInt(nil, 0)
	payload = mcproto.AppendVarInt(payload, 767)
	payload = mcproto.AppendVarInt(payload, int32(len(host)))
	payload = append(payload, host...)
	payload = binary.BigEndian.AppendUint16(payload, 25565)
	payload = mcproto.AppendVarInt(payload, int32(state))
	return mcproto.EncodePacket(payload)
}

func loginPacket(name string) []byte {
	payload := mcproto.AppendVarInt(nil, 0)
	payload = mcproto.AppendVarInt(payload, int32(len(name)))
	payload = append(payload, name...)
	// Modern login-start has an opaque fixed-width UUID.
	payload = append(payload, bytes.Repeat([]byte{0xA5}, 16)...)
	return mcproto.EncodePacket(payload)
}

func dialClient(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	return conn
}

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestOpaqueRelayPreservesOriginalFramesAndHalfClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	controller := &fakeController{state: control.Ready, healthy: true}
	server, address, _ := startGateway(t, Config{BackendAddress: backend.Addr().String()}, controller)
	handshake := handshakePacket("friends.example\x00FML3\x00", 2)
	// Noncanonical but valid VarInt length: parsing must never normalize bytes
	// on the backend connection. The original FML suffix must also survive.
	handshake = append([]byte{handshake[0] | 0x80, 0}, handshake[1:]...)
	opaque := bytes.Repeat([]byte{0xFE, 0, 0x80, 0xFF, 7, 9, 0xAA}, 4096)
	expected := append(append(append([]byte(nil), handshake...), loginPacket("Mesut")...), opaque...)
	reply := bytes.Repeat([]byte{0x80, 0xFF, 0, 0xFE, 42, 13}, 2048)
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		got, err := io.ReadAll(conn)
		if err != nil {
			backendDone <- err
			return
		}
		if !bytes.Equal(got, expected) {
			backendDone <- fmt.Errorf("backend bytes changed: got %d, want %d", len(got), len(expected))
			return
		}
		backendDone <- writeAll(conn, reply)
	}()
	client := dialClient(t, address)
	if err := writeAll(client, expected); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("backend reply changed: got %d, want %d", len(got), len(reply))
	}
	if err := <-backendDone; err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return controller.Snapshot().ActiveConnections == 0 })
	if server.Stats().Routed != 1 {
		t.Fatalf("stats = %+v", server.Stats())
	}
	if controller.wakeCount() != 0 {
		t.Fatal("a ready login should not wake the backend")
	}
}

func TestStatusNeverWakesAndPreservesModMetadata(t *testing.T) {
	cache := json.RawMessage(`{"version":{"name":"1.21.1","protocol":767},"players":{"max":5,"online":2,"sample":[{"name":"stale"}],"customPlayerFlag":{"v":7}},"description":"Friends","isModded":true,"betterStatus":{"pack":"ATM10","bytes":[0,255]},"preventsChatReports":true}`)
	controller := &fakeController{state: control.Asleep, healthy: true, cache: cache}
	server, address, _ := startGateway(t, Config{}, controller)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		got, err := mcproto.QueryStatus(ctx, address, 767)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(got, &data); err != nil {
			t.Fatal(err)
		}
		if string(data["betterStatus"]) != `{"pack":"ATM10","bytes":[0,255]}` {
			t.Fatalf("mod metadata was changed: %s", data["betterStatus"])
		}
		if string(data["isModded"]) != "true" || string(data["preventsChatReports"]) != "true" {
			t.Fatalf("flags were lost: %s", got)
		}
		var players map[string]json.RawMessage
		_ = json.Unmarshal(data["players"], &players)
		if string(players["max"]) != "5" || string(players["online"]) != "0" || string(players["sample"]) != "[]" || string(players["customPlayerFlag"]) != `{"v":7}` {
			t.Fatalf("players mutated incorrectly: %s", data["players"])
		}
	}
	if controller.wakeCount() != 0 {
		t.Fatal("status ping woke the backend")
	}
	if !bytes.Equal(controller.CachedStatus(), cache) {
		t.Fatal("status rendering mutated cached JSON")
	}
	if server.Stats().StatusRequests != 5 {
		t.Fatalf("stats = %+v", server.Stats())
	}
}

func TestSleepingLoginWakesAndGetsFriendlyDisconnect(t *testing.T) {
	controller := &fakeController{state: control.Asleep}
	server, address, _ := startGateway(t, Config{BootMessage: "Server is booting. Please reconnect shortly."}, controller)
	client := dialClient(t, address)
	packet := append(handshakePacket("friends.example", 2), loginPacket("Mzar")...)
	if err := writeAll(client, packet); err != nil {
		t.Fatal(err)
	}
	_, payload, err := mcproto.ReadPacket(client, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < 3 || payload[0] != 0 {
		t.Fatalf("not a login disconnect: %x", payload)
	}
	if !bytes.Contains(payload, []byte("Server is booting. Please reconnect shortly.")) {
		t.Fatalf("missing boot message: %q", payload)
	}
	if controller.wakeCount() != 1 || controller.Snapshot().State != control.Starting {
		t.Fatalf("wake state: %+v, wakes %d", controller.Snapshot(), controller.wakeCount())
	}
	if server.Stats().Routed != 0 {
		t.Fatal("sleeping login was forwarded")
	}
}

func TestHandshakeWithoutLoginCannotWake(t *testing.T) {
	controller := &fakeController{state: control.Asleep}
	server, address, _ := startGateway(t, Config{HandshakeTimeout: 40 * time.Millisecond}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, handshakePacket("friends.example", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("handshake-only connection was not closed")
	}
	if controller.wakeCount() != 0 {
		t.Fatal("handshake-only connection woke backend")
	}
	if server.Stats().InvalidRequests != 1 {
		t.Fatalf("stats = %+v", server.Stats())
	}
}

func TestMalformedLoginCannotWake(t *testing.T) {
	controller := &fakeController{state: control.Asleep}
	_, address, _ := startGateway(t, Config{}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, append(handshakePacket("friends.example", 2), []byte{1, 5}...)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("invalid login was not closed")
	}
	if controller.wakeCount() != 0 {
		t.Fatal("malformed login woke backend")
	}
}

func TestUnknownPlayerHealthBlocksBackendDial(t *testing.T) {
	controller := &fakeController{state: control.Ready, healthy: false}
	server, address, _ := startGateway(t, Config{}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, append(handshakePacket("friends.example", 2), loginPacket("Mzar")...)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := mcproto.ReadPacket(client, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("temporarily unavailable")) {
		t.Fatalf("wrong failure message: %s", payload)
	}
	if server.Stats().BackendErrors != 0 || controller.wakeCount() != 0 {
		t.Fatal("unsafe backend access with failed player health")
	}
}

func TestStoppingLoginRequestsRetryWithoutJoining(t *testing.T) {
	controller := &fakeController{state: control.Stopping}
	_, address, _ := startGateway(t, Config{}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, append(handshakePacket("friends.example", 2), loginPacket("Mzar")...)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := mcproto.ReadPacket(client, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("saving and shutting down")) {
		t.Fatalf("wrong stopping message: %s", payload)
	}
	if controller.Snapshot().ActiveConnections != 0 {
		t.Fatal("join acquired during shutdown")
	}
}

func TestCancellationClosesRelayAndReleasesLease(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	controller := &fakeController{state: control.Ready, healthy: true}
	server, address, cancel := startGateway(t, Config{BackendAddress: backend.Addr().String()}, controller)
	backendAccepted := make(chan struct{})
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		close(backendAccepted)
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = io.Copy(io.Discard, conn)
		backendDone <- err
	}()
	client := dialClient(t, address)
	if err := writeAll(client, append(handshakePacket("friends.example", 2), loginPacket("Mzar")...)); err != nil {
		t.Fatal(err)
	}
	// A join lease is acquired before DialContext. Cancelling at that point
	// can correctly abort the dial and leave the fixture waiting in Accept.
	// Exercise cancellation of an established relay instead.
	select {
	case <-backendAccepted:
	case err := <-backendDone:
		t.Fatalf("backend stopped before accepting the relay: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("backend did not accept the relay")
	}
	eventually(t, func() bool { return server.Stats().Routed == 1 && controller.Snapshot().ActiveConnections == 1 })
	cancel()
	_, _ = io.ReadAll(client)
	eventually(t, func() bool { return controller.Snapshot().ActiveConnections == 0 })
	select {
	case err := <-backendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend relay did not close after cancellation")
	}
}

func TestConnectionLimitRejectsSlowlorisOverflow(t *testing.T) {
	controller := &fakeController{state: control.Asleep}
	server, address, _ := startGateway(t, Config{MaxConnections: 1, HandshakeTimeout: 2 * time.Second}, controller)
	first := dialClient(t, address)
	if err := writeAll(first, []byte{0x80}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { server.mu.Lock(); defer server.mu.Unlock(); return len(server.connections) == 1 })
	second := dialClient(t, address)
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-capacity connection was not closed")
	}
	if server.Stats().CapacityRejected != 1 {
		t.Fatalf("stats = %+v", server.Stats())
	}
	if controller.wakeCount() != 0 {
		t.Fatal("slowloris connection woke backend")
	}
}

func TestStatusReadyRetainsDescriptionAndCustomPlayers(t *testing.T) {
	controller := &fakeController{state: control.Ready, healthy: true, players: 2, cache: json.RawMessage(`{"description":{"text":"Friends","color":"blue"},"players":{"online":0,"max":5,"custom":123},"version":{"protocol":767,"name":"1.21.1"}}`)}
	server := New(Config{}, controller, nil)
	got := string(server.status())
	if !strings.Contains(got, `"color":"blue"`) || !strings.Contains(got, `"online":2`) || !strings.Contains(got, `"custom":123`) {
		t.Fatalf("ready status changed incorrectly: %s", got)
	}
}
