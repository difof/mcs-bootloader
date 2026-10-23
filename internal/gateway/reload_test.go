package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/mcproto"
)

func TestReconfigurePreservesOpaqueRelayAndChangesAdmissionLimit(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	preface := append(handshakePacket("friends.example\x00FML3\x00", 2), loginPacket("Mesut")...)
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		got := make([]byte, len(preface))
		if _, err := io.ReadFull(conn, got); err != nil {
			backendDone <- err
			return
		}
		if !bytes.Equal(got, preface) {
			backendDone <- fmt.Errorf("original modded preface changed")
			return
		}
		_, err = io.Copy(conn, conn)
		backendDone <- err
	}()
	t.Cleanup(func() {
		backend.Close()
		select {
		case err := <-backendDone:
			if err != nil {
				t.Errorf("fake backend: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("fake backend did not exit")
		}
	})
	controller := &fakeController{state: control.Ready, healthy: true}
	server, address, _ := startGateway(t, Config{BackendAddress: backend.Addr().String(), MaxConnections: 2}, controller)
	client := dialClient(t, address)
	if err := writeAll(client, preface); err != nil {
		t.Fatal(err)
	}
	assertRelay := func(marker byte) {
		t.Helper()
		payload := bytes.Repeat([]byte{0, 0xFF, 0x80, 0xFE, marker}, 4096)
		if err := writeAll(client, payload); err != nil {
			t.Fatalf("live relay write: %v", err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(client, got); err != nil {
			t.Fatalf("live relay read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("opaque gameplay bytes changed")
		}
	}
	assertRelay(1)
	second := dialClient(t, address)
	eventually(t, func() bool { return connectionCount(server) == 2 })
	updated := server.ConfigSnapshot()
	updated.MaxConnections = 1
	updated.HandshakeTimeout = time.Second
	updated.DialTimeout = 200 * time.Millisecond
	updated.BootMessage = "Reloaded boot message"
	updated.SleepingMOTD = "Reloaded sleeping message"
	if err := server.Reconfigure(updated); err != nil {
		t.Fatal(err)
	}
	assertRelay(2)
	if connectionCount(server) != 2 || controller.Snapshot().ActiveConnections != 1 {
		t.Fatal("lowering the admission limit closed an existing connection or join lease")
	}
	rejected := dialClient(t, address)
	var one [1]byte
	if _, err := rejected.Read(one[:]); err == nil {
		t.Fatal("new connection was admitted above the reduced limit")
	}
	eventually(t, func() bool { return server.Stats().CapacityRejected == 1 })
	second.Close()
	eventually(t, func() bool { return connectionCount(server) == 1 })
	updated.MaxConnections = 3
	if err := server.Reconfigure(updated); err != nil {
		t.Fatal(err)
	}
	third := dialClient(t, address)
	eventually(t, func() bool { return connectionCount(server) == 2 })
	assertRelay(3)
	third.Close()
	client.Close()
	eventually(t, func() bool { return controller.Snapshot().ActiveConnections == 0 })
	if server.Stats().Routed != 1 || controller.wakeCount() != 0 {
		t.Fatalf("reload reconnected or woke the backend: %+v", server.Stats())
	}
}

func connectionCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.connections)
}

func TestReconfigureUpdatesStatusAndCopiesFallbackJSON(t *testing.T) {
	controller := &fakeController{state: control.Asleep}
	fallback := json.RawMessage(`{"version":{"protocol":767,"name":"1.21.1"},"players":{"max":5},"isModded":true,"betterStatus":{"pack":"old"}}`)
	server, address, _ := startGateway(t, Config{SleepingMOTD: "Before reload", FallbackStatus: fallback}, controller)
	fallback[0] = '!'
	query := func() map[string]json.RawMessage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		raw, err := mcproto.QueryStatus(ctx, address, 767)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	before := query()
	if string(before["description"]) != `{"text":"Before reload"}` || string(before["isModded"]) != "true" {
		t.Fatalf("initial fallback shared mutable input: %v", before)
	}
	updated := server.ConfigSnapshot()
	updated.SleepingMOTD = "After reload"
	updated.FallbackStatus = json.RawMessage(`{"version":{"protocol":767,"name":"1.21.1"},"players":{"max":5},"isModded":true,"betterStatus":{"pack":"new","custom":[0,255]}}`)
	if err := server.Reconfigure(updated); err != nil {
		t.Fatal(err)
	}
	updated.FallbackStatus[0] = '!'
	snapshot := server.ConfigSnapshot()
	snapshot.FallbackStatus[0] = '!'
	after := query()
	if string(after["description"]) != `{"text":"After reload"}` || string(after["betterStatus"]) != `{"pack":"new","custom":[0,255]}` || string(after["isModded"]) != "true" {
		t.Fatalf("updated message/opaque fallback not retained: %v", after)
	}
	if controller.wakeCount() != 0 {
		t.Fatal("status update woke the server")
	}
}

func TestReconfigureRejectsInvalidOrStartupOnlyChanges(t *testing.T) {
	server := New(Config{BackendAddress: "127.0.0.1:25566"}, &fakeController{state: control.Asleep}, nil)
	base := server.ConfigSnapshot()
	changes := []func(*Config){
		func(c *Config) { c.BackendAddress = "127.0.0.1:25567" },
		func(c *Config) { c.Protocol++ },
		func(c *Config) { c.BackendAddress = "invalid" },
		func(c *Config) { c.BackendAddress = "127.0.0.1:0" },
		func(c *Config) { c.MaxConnections = 0 },
		func(c *Config) { c.MaxConnections = 4097 },
		func(c *Config) { c.HandshakeTimeout = 0 },
		func(c *Config) { c.DialTimeout = -time.Second },
		func(c *Config) { c.BootMessage = "" },
		func(c *Config) { c.FallbackStatus = json.RawMessage(`[]`) },
		func(c *Config) { c.FallbackStatus = json.RawMessage(`null`) },
		func(c *Config) { c.FallbackStatus = json.RawMessage(`{`) },
	}
	before, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for i, change := range changes {
		invalid := base
		change(&invalid)
		if err := server.Reconfigure(invalid); err == nil {
			t.Errorf("invalid or startup-only configuration %d was accepted", i)
		}
		after, err := json.Marshal(server.ConfigSnapshot())
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("rejected update %d changed configuration: %s, %v", i, after, err)
		}
	}
}

func TestConcurrentReconfigureUsesConsistentStatusSnapshots(t *testing.T) {
	server := New(Config{BackendAddress: "127.0.0.1:25566", SleepingMOTD: "generation 0", FallbackStatus: json.RawMessage(`{"generation":0}`)}, &fakeController{state: control.Asleep}, nil)
	base := server.ConfigSnapshot()
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 50; i++ {
				generation := worker*100 + i + 1
				updated := base
				updated.SleepingMOTD = fmt.Sprintf("generation %d", generation)
				updated.FallbackStatus = json.RawMessage(fmt.Sprintf(`{"generation":%d,"isModded":true}`, generation))
				updated.MaxConnections = generation%64 + 1
				if err := server.Reconfigure(updated); err != nil {
					t.Errorf("concurrent reload: %v", err)
					return
				}
				// Mutation of caller-owned data must not affect live readers.
				updated.FallbackStatus[0] = '!'
				var status struct {
					Generation  int `json:"generation"`
					Description struct {
						Text string `json:"text"`
					} `json:"description"`
				}
				if err := json.Unmarshal(server.status(), &status); err != nil {
					t.Errorf("status decode: %v", err)
					return
				}
				if status.Description.Text != fmt.Sprintf("generation %d", status.Generation) {
					t.Errorf("status mixed two configurations: %+v", status)
					return
				}
				snapshot := server.ConfigSnapshot()
				snapshot.FallbackStatus[0] = '!'
			}
		}(worker)
	}
	workers.Wait()
}
