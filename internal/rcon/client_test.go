package rcon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "test-secret-not-for-logs"

func fakeRCON(t *testing.T, handler func(net.Conn) error) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		done <- handler(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("fake RCON: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("fake RCON did not exit")
		}
	})
	return New(ln.Addr().String(), testPassword, time.Second)
}

func authenticate(conn net.Conn, emptyFirst bool) error {
	p, err := readPacket(conn)
	if err != nil {
		return err
	}
	if p.kind != 3 || p.id != authID || string(p.body) != testPassword {
		return errors.New("invalid auth request")
	}
	if emptyFirst {
		if err := writePacket(conn, authID, 0, ""); err != nil {
			return err
		}
	}
	return writePacket(conn, authID, 2, "")
}

func readRequest(conn net.Conn, wanted string) error {
	p, err := readPacket(conn)
	if err != nil {
		return err
	}
	if p.kind != 2 || p.id != commandID || string(p.body) != wanted {
		return fmt.Errorf("unexpected command envelope")
	}
	return nil
}

func readBoundary(conn net.Conn) error {
	p, err := readPacket(conn)
	if err != nil {
		return err
	}
	if p.kind != 0 || p.id != boundaryID || len(p.body) != 0 {
		return errors.New("missing response boundary")
	}
	return nil
}

func finish(conn net.Conn, chunks ...string) error {
	for _, chunk := range chunks {
		if err := writePacket(conn, commandID, 0, chunk); err != nil {
			return err
		}
	}
	if err := readBoundary(conn); err != nil {
		return err
	}
	return writePacket(conn, boundaryID, 0, "Unknown request 0")
}

func TestPlayersCollectsFragmentsAndSourceEmptyAuthReply(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, true); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		return finish(conn, "There are ", "2 of a max of 5 players online: ", "Mzar, Mesut")
	})
	n, err := c.Players(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("players=%d err=%v", n, err)
	}
}

func TestZeroPlayersRequiresCompletedResponse(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		// A complete-looking first frame must not be accepted without the
		// boundary, since a later fragment could contradict zero players.
		return writePacket(conn, commandID, 0, "There are 0 of a max of 5 players online: ")
	})
	if n, err := c.Players(context.Background()); err == nil {
		t.Fatalf("accepted incomplete zero-player reply: %d", n)
	}
}

func TestEmptyPlayerList(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		return finish(conn, "There are 0 of a max of 5 players online: ")
	})
	n, err := c.Players(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("players=%d err=%v", n, err)
	}
}

func TestAuthenticationFailureDoesNotLeakSecretOrSendCommand(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		p, err := readPacket(conn)
		if err != nil {
			return err
		}
		if p.kind != 3 {
			return errors.New("not auth")
		}
		if err := writePacket(conn, -1, 2, ""); err != nil {
			return err
		}
		_, err = readPacket(conn)
		if !errors.Is(err, io.EOF) {
			return errors.New("client sent data after failed authentication")
		}
		return nil
	})
	_, err := c.Command(context.Background(), "list")
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("expected auth rejection, got %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("secret leaked in error")
	}
}

func TestPlayerParsingRejectsUnknownOrContradictoryReplies(t *testing.T) {
	invalid := []string{
		"", "Unknown command", "You do not have permission to use this command",
		"There are 0 players online", "There are -1 of a max of 5 players online:",
		"There are 0 of a max of 5 players online: unexpected",
		"There are 1 of a max of 5 players online:",
		"There are 9999999999999999999999999999999 of a max of 5 players online: x",
		"There are 0 of a max of 5 players online:\nERROR",
	}
	for _, reply := range invalid {
		if _, err := parsePlayers(reply); err == nil {
			t.Fatalf("accepted ambiguous list %q", reply)
		}
	}
	for _, reply := range []string{"There are 0 of a max of 5 players online:", "§aThere are 0 of a max of 5 players online: §r\n"} {
		n, err := parsePlayers(reply)
		if n != 0 || err != nil {
			t.Fatalf("valid empty list rejected: %v", err)
		}
	}
}

func TestStopAcknowledgementWithImmediateEOF(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "stop"); err != nil {
			return err
		}
		return writePacket(conn, commandID, 0, "Stopping the server")
	})
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopDoesNotTreatPermissionErrorAsSuccess(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "stop"); err != nil {
			return err
		}
		return finish(conn, "Unknown or incomplete command")
	})
	if err := c.Stop(context.Background()); err == nil {
		t.Fatal("unacknowledged stop accepted")
	}
}

func TestCancellationInterruptsRead(t *testing.T) {
	ready := make(chan struct{})
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		close(ready)
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Command(ctx, "list"); result <- err }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("RCON request not received")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt request")
	}
}

func TestPacketBoundsTerminatorsAndTruncation(t *testing.T) {
	var valid bytes.Buffer
	if err := writePacket(&valid, 7, 0, "response"); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{}, valid.Bytes()...)
	malformed := [][]byte{
		{}, {1, 0, 0, 0}, {0xff, 0xff, 0xff, 0x7f}, wire[:len(wire)-1],
	}
	wrongTerminator := append([]byte{}, wire...)
	wrongTerminator[len(wrongTerminator)-1] = 1
	malformed = append(malformed, wrongTerminator)
	for _, raw := range malformed {
		if _, err := readPacket(bytes.NewReader(raw)); err == nil {
			t.Fatalf("accepted invalid frame %x", raw)
		}
	}
	p, err := readPacket(bytes.NewReader(wire))
	if err != nil || p.id != 7 || p.kind != 0 || string(p.body) != "response" {
		t.Fatalf("valid frame: %+v %v", p, err)
	}
	if binary.LittleEndian.Uint32(wire) != uint32(len(wire)-4) {
		t.Fatal("incorrect little-endian framing")
	}
}

func TestResponseIDMismatch(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		return writePacket(conn, 99, 0, "There are 0 of a max of 5 players online:")
	})
	if _, err := c.Players(context.Background()); err == nil {
		t.Fatal("accepted unrelated reply")
	}
}

func TestConcurrentCommandsUseIndependentConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if listener, ok := ln.(*net.TCPListener); ok {
		_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	}
	const workers = 8
	errorsOut := make(chan error, workers*2)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			conn, err := ln.Accept()
			if err != nil {
				errorsOut <- err
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if err := authenticate(conn, false); err != nil {
					errorsOut <- err
					return
				}
				if err := readRequest(conn, "list"); err != nil {
					errorsOut <- err
					return
				}
				if err := finish(conn, "There are 1 of a max of 5 players online: player"); err != nil {
					errorsOut <- err
				}
			}()
		}
		wg.Wait()
	}()
	c := New(ln.Addr().String(), testPassword, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := c.Players(context.Background())
			if err != nil {
				errorsOut <- err
			} else if n != 1 {
				errorsOut <- errors.New("incorrect player count")
			}
		}()
	}
	wg.Wait()
	<-serverDone
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
}

// sequencingServer models the Minecraft readers that accept only one request
// per receive cycle. It deterministically rejects a second request before the
// first response was consumed, without relying on TCP packet timing/coalescing.
type sequencingServer struct {
	input             bytes.Buffer
	replies           []string
	firstRemaining    int
	firstRead         bool
	boundaryAttempts  int
	failBoundaryWrite bool
}

func (s *sequencingServer) Read(p []byte) (int, error) {
	n, err := s.input.Read(p)
	if s.firstRemaining > 0 {
		s.firstRemaining -= n
		if s.firstRemaining <= 0 {
			s.firstRead = true
		}
	}
	return n, err
}

func (s *sequencingServer) Write(raw []byte) (int, error) {
	r := bytes.NewReader(raw)
	p, err := readPacket(r)
	if err != nil {
		return 0, err
	}
	if r.Len() != 0 {
		return 0, errors.New("coalesced RCON requests are unsupported")
	}
	switch p.id {
	case authID:
		if p.kind != 3 {
			return 0, errors.New("invalid auth type")
		}
		if err := writePacket(&s.input, authID, 2, ""); err != nil {
			return 0, err
		}
	case commandID:
		if p.kind != 2 || len(s.replies) == 0 {
			return 0, errors.New("invalid command")
		}
		for i, reply := range s.replies {
			before := s.input.Len()
			if err := writePacket(&s.input, commandID, 0, reply); err != nil {
				return 0, err
			}
			if i == 0 {
				s.firstRemaining = s.input.Len() - before
			}
		}
	case boundaryID:
		s.boundaryAttempts++
		if !s.firstRead {
			return 0, errors.New("pipelined boundary arrived before first command response")
		}
		if s.failBoundaryWrite {
			return 0, io.ErrClosedPipe
		}
		if err := writePacket(&s.input, boundaryID, 0, "Unknown request 0"); err != nil {
			return 0, err
		}
	default:
		return 0, errors.New("unexpected request ID")
	}
	return len(raw), nil
}

func TestBoundaryWaitsForFirstResponseWithoutPipelining(t *testing.T) {
	s := &sequencingServer{replies: []string{"There are ", "0 of a max of 5 players online: "}}
	c := New("unused", testPassword, time.Second)
	response, err := c.exchange(s, "list")
	if err != nil {
		t.Fatal(err)
	}
	if response != "There are 0 of a max of 5 players online: " || !s.firstRead || s.boundaryAttempts != 1 {
		t.Fatalf("incorrect sequencing: response=%q first_read=%v boundaries=%d", response, s.firstRead, s.boundaryAttempts)
	}
}

func TestAcknowledgedStopSurvivesBoundaryWriteFailure(t *testing.T) {
	s := &sequencingServer{replies: []string{"Stopping the server"}, failBoundaryWrite: true}
	c := New("unused", testPassword, time.Second)
	response, err := c.exchange(s, "stop")
	if err != nil || !isStopReply(response) {
		t.Fatalf("lost affirmative stop: response=%q err=%v", response, err)
	}
	if s.boundaryAttempts != 1 {
		t.Fatal("did not exercise boundary write failure")
	}
}

func TestIncompleteListStillFailsAfterBoundaryWriteFailure(t *testing.T) {
	s := &sequencingServer{replies: []string{"There are 0 of a max of 5 players online: "}, failBoundaryWrite: true}
	c := New("unused", testPassword, time.Second)
	if _, err := c.exchange(s, "list"); err == nil {
		t.Fatal("accepted an incomplete zero-player list")
	}
}

func FuzzReadRCONPacket(f *testing.F) {
	var wire bytes.Buffer
	_ = writePacket(&wire, 1, 0, "reply")
	f.Add(wire.Bytes())
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = readPacket(bytes.NewReader(b)) })
}
