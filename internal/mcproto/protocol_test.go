package mcproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func handshakeFrame(host string, next int32) []byte {
	p := AppendVarInt([]byte{0}, 767)
	p = AppendVarInt(p, int32(len(host)))
	p = append(p, host...)
	p = binary.BigEndian.AppendUint16(p, 25565)
	p = AppendVarInt(p, next)
	return EncodePacket(p)
}

func loginFrame(name string) []byte {
	p := AppendVarInt([]byte{0}, int32(len(name)))
	p = append(p, name...)
	p = append(p, bytes.Repeat([]byte{0xab}, 16)...)
	return EncodePacket(p)
}

type byteReader struct{ r io.Reader }

func (b byteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return b.r.Read(p)
}

func TestHandshakeRetainsOriginalModdedBytesWithoutReadAhead(t *testing.T) {
	frame := handshakeFrame("server.test\x00FML3\x00opaque-extension", 2)
	// A legal noncanonical length must also survive exactly as received.
	frame = append([]byte{frame[0] | 0x80, 0}, frame[1:]...)
	login := loginFrame("Player_01")
	r := bytes.NewReader(append(append([]byte{}, frame...), login...))
	h, err := ReadHandshake(byteReader{r})
	if err != nil {
		t.Fatal(err)
	}
	if h.Protocol != 767 || h.Port != 25565 || h.NextState != 2 || h.Host != "server.test\x00FML3\x00opaque-extension" {
		t.Fatalf("unexpected handshake: %+v", h)
	}
	if !bytes.Equal(h.Raw, frame) {
		t.Fatal("original frame was rewritten")
	}
	if r.Len() != len(login) {
		t.Fatal("read consumed bytes from next packet")
	}
	raw, name, err := ReadLoginStart(r, 767)
	if err != nil || name != "Player_01" || !bytes.Equal(raw, login) {
		t.Fatalf("login not preserved: name=%q err=%v", name, err)
	}
}

func TestPacketRejectsInvalidLengthsAndTruncation(t *testing.T) {
	cases := [][]byte{
		{}, {0}, {0xff, 0xff, 0xff, 0xff, 0x0f},
		{0x80, 0x80, 0x80, 0x80, 0x10}, {0x80, 0x80, 0x80, 0x80, 0x80, 0},
		{0x81, 0x80, 0x80, 0x80, 0x08}, {3, 0, 1}, {17},
	}
	for _, input := range cases {
		if _, _, err := ReadPacket(bytes.NewReader(input), 16); err == nil {
			t.Fatalf("accepted malformed frame %x", input)
		}
	}
}

func TestHandshakeValidation(t *testing.T) {
	for _, state := range []int32{0, 4, -1} {
		if _, err := ReadHandshake(bytes.NewReader(handshakeFrame("host", state))); err == nil {
			t.Fatalf("accepted state %d", state)
		}
	}
	if _, err := ReadHandshake(bytes.NewReader(handshakeFrame("", 2))); err == nil {
		t.Fatal("accepted empty host")
	}
	if _, err := ReadHandshake(bytes.NewReader(handshakeFrame(string([]byte{0xff}), 2))); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	for _, state := range []int32{1, 2, 3} {
		if _, err := ReadHandshake(bytes.NewReader(handshakeFrame("host", state))); err != nil {
			t.Fatalf("rejected state %d: %v", state, err)
		}
	}
}

func TestLoginStartRequiresCompleteEnvelope(t *testing.T) {
	for _, name := range []string{"", "12345678901234567", "has space", "new\nline", "nul\x00name"} {
		if _, _, err := ReadLoginStart(bytes.NewReader(loginFrame(name)), 767); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	frame := loginFrame("player")
	_, p, err := ReadPacket(bytes.NewReader(frame), 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{p[:len(p)-1], append(append([]byte{}, p...), 0)} {
		if _, _, err := ReadLoginStart(bytes.NewReader(EncodePacket(body)), 767); err == nil {
			t.Fatal("accepted incomplete UUID or trailing data")
		}
	}
	if _, _, err := ReadLoginStart(bytes.NewReader(frame), 763); err == nil {
		t.Fatal("accepted an unsupported login layout")
	}
}

func TestDisconnectUsesLoginJSONString(t *testing.T) {
	var wire bytes.Buffer
	message := "Server is booting up.\nPlease reconnect shortly."
	if err := WriteDisconnect(&wire, 767, message); err != nil {
		t.Fatal(err)
	}
	_, p, err := ReadPacket(&wire, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if p[0] != 0 {
		t.Fatal("incorrect login disconnect packet ID")
	}
	r := bytes.NewReader(p[1:])
	s, err := readString(r, 1024)
	if err != nil || r.Len() != 0 {
		t.Fatalf("not a length-prefixed JSON string: %v", err)
	}
	var component map[string]string
	if err := json.Unmarshal([]byte(s), &component); err != nil {
		t.Fatal(err)
	}
	if component["text"] != message {
		t.Fatalf("incorrect component: %v", component)
	}
}

func TestStatusPreservesUnknownJSONAndEchoesPing(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	_ = server.SetDeadline(time.Now().Add(time.Second))
	_ = client.SetDeadline(time.Now().Add(time.Second))
	status := json.RawMessage(`{"version":{"name":"1.21.1","protocol":767},"players":{"online":0,"max":5},"isModded":true,"betterStatus":{"opaque":[1,{"new":"field"}]}}`)
	done := make(chan error, 1)
	go func() { done <- HandleStatus(server, status) }()
	if _, err := client.Write([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := ReadPacket(client, MaxStatus+6)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(payload[1:])
	s, err := readString(r, MaxStatus)
	if err != nil || s != string(status) {
		t.Fatalf("status changed: %s, %v", s, err)
	}
	ping := []byte{9, 1, 0x80, 1, 2, 3, 4, 5, 6, 7}
	if _, err := client.Write(ping); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(ping))
	if _, err := io.ReadFull(client, echo); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echo, ping) {
		t.Fatal("ping not echoed verbatim")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriteStatusRejectsNonObjectAndOversize(t *testing.T) {
	for _, b := range []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`{"bad":`), append([]byte(`{"text":"`), bytes.Repeat([]byte{'a'}, MaxStatus)...)} {
		if err := WriteStatus(io.Discard, b); err == nil {
			t.Fatal("accepted invalid status")
		}
	}
}

func TestQueryStatusReturnsCompleteMetadata(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	status := json.RawMessage(`{"version":{"name":"1.21.1","protocol":767},"players":{"online":0,"max":5},"isModded":true,"betterStatus":{"extension":"retained"}}`)
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		h, err := ReadHandshake(conn)
		if err != nil {
			done <- err
			return
		}
		if h.NextState != 1 || h.Protocol != 767 {
			done <- errors.New("not a status handshake")
			return
		}
		done <- HandleStatus(conn, status)
	}()
	got, err := QueryStatus(context.Background(), ln.Addr().String(), 767)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, status) {
		t.Fatal("unknown status fields changed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQueryStatusCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := ReadHandshake(conn); err != nil {
			return
		}
		if _, _, err := ReadPacket(conn, 16); err != nil {
			return
		}
		close(ready)
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := QueryStatus(ctx, ln.Addr().String(), 767); result <- err }()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("status request not received")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt network read")
	}
	<-done
}

func FuzzReadHandshake(f *testing.F) {
	f.Add(handshakeFrame("test\x00FML3\x00", 2))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ReadHandshake(bytes.NewReader(b)) })
}
