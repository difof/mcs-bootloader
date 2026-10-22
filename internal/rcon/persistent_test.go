package rcon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func persistentFixture(t *testing.T, handler func(net.Listener) error) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { done <- handler(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("persistent RCON fixture: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("persistent RCON fixture did not exit")
		}
	})
	return ln.Addr().String()
}

func acceptAuthenticated(ln net.Listener) (net.Conn, error) {
	conn, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := authenticate(conn, false); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func persistentRequest(conn net.Conn, command string) (int32, error) {
	p, err := readPacket(conn)
	if err != nil {
		return 0, err
	}
	if p.kind != 2 || p.id < 2 || p.id%2 != 0 || string(p.body) != command {
		return 0, errors.New("unexpected persistent command envelope")
	}
	return p.id, nil
}

func persistentReply(conn net.Conn, id int32, chunks ...string) error {
	for _, chunk := range chunks {
		if err := writePacket(conn, id, 0, chunk); err != nil {
			return err
		}
	}
	p, err := readPacket(conn)
	if err != nil {
		return err
	}
	if p.kind != 0 || p.id != id+1 || len(p.body) != 0 {
		return errors.New("persistent response boundary ID mismatch")
	}
	return writePacket(conn, p.id, 0, "Unknown request 0")
}

func TestPersistentClientReusesOneAuthenticationAndUniqueIDs(t *testing.T) {
	address := persistentFixture(t, func(ln net.Listener) error {
		conn, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer conn.Close()
		for i := 0; i < 3; i++ {
			id, err := persistentRequest(conn, "list")
			if err != nil {
				return err
			}
			if id != int32(2+i*2) {
				return fmt.Errorf("request IDs reused or skipped: %d", id)
			}
			if err := persistentReply(conn, id, "There are ", "0 of a max of 5 players online: "); err != nil {
				return err
			}
		}
		_, err = readPacket(conn)
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("expected client Close after repeated requests: %w", err)
		}
		return nil
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	for i := 0; i < 3; i++ {
		n, err := c.Players(context.Background())
		if err != nil || n != 0 {
			t.Fatalf("request %d: players=%d err=%v", i, n, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Players(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed client returned %v", err)
	}
}

func TestPersistentConcurrentCallsAreSerialized(t *testing.T) {
	const callers = 16
	address := persistentFixture(t, func(ln net.Listener) error {
		conn, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer conn.Close()
		for i := 0; i < callers; i++ {
			id, err := persistentRequest(conn, "list")
			if err != nil {
				return err
			}
			if id != int32(2+i*2) {
				return errors.New("concurrent requests interleaved or reused IDs")
			}
			if err := persistentReply(conn, id, "There are 0 of a max of 5 players online: "); err != nil {
				return err
			}
		}
		return nil
	})
	c := NewPersistent(address, testPassword, 2*time.Second)
	defer c.Close()
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := c.Players(context.Background())
			if err != nil {
				errs <- err
			} else if n != 0 {
				errs <- errors.New("incorrect count")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPersistentFailureReturnsBeforeLaterCallReconnects(t *testing.T) {
	var accepts atomic.Int32
	address := persistentFixture(t, func(ln net.Listener) error {
		first, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		accepts.Add(1)
		if _, err := persistentRequest(first, "list"); err != nil {
			first.Close()
			return err
		}
		// The request was received, but its reply was lost. It must never be
		// silently replayed as part of this same Command call.
		first.Close()
		second, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer second.Close()
		accepts.Add(1)
		id, err := persistentRequest(second, "list")
		if err != nil {
			return err
		}
		return persistentReply(second, id, "There are 1 of a max of 5 players online: player")
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	if _, err := c.Players(context.Background()); err == nil {
		t.Fatal("lost response was automatically replayed or accepted")
	}
	if n := accepts.Load(); n != 1 {
		t.Fatalf("failed command created %d connections", n)
	}
	n, err := c.Players(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("later reconnect: players=%d err=%v", n, err)
	}
	if n := accepts.Load(); n != 2 {
		t.Fatalf("expected second connection, got %d", n)
	}
}

func TestPersistentUncertainStopIsNotReplayed(t *testing.T) {
	var stops atomic.Int32
	address := persistentFixture(t, func(ln net.Listener) error {
		first, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		if _, err := persistentRequest(first, "stop"); err != nil {
			first.Close()
			return err
		}
		stops.Add(1)
		first.Close()
		second, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer second.Close()
		// A later explicit health request is allowed; a replayed stop is not.
		id, err := persistentRequest(second, "list")
		if err != nil {
			return err
		}
		return persistentReply(second, id, "There are 0 of a max of 5 players online: ")
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	if err := c.Stop(context.Background()); err == nil {
		t.Fatal("uncertain stop was accepted")
	}
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatal("stop was replayed")
	}
}

func TestPersistentAcknowledgedStopDiscardsConnection(t *testing.T) {
	address := persistentFixture(t, func(ln net.Listener) error {
		first, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer first.Close()
		id, err := persistentRequest(first, "stop")
		if err != nil {
			return err
		}
		if err := persistentReply(first, id, "Stopping the server"); err != nil {
			return err
		}
		if _, err := readPacket(first); !errors.Is(err, io.EOF) {
			return errors.New("stop left its connection cached")
		}
		second, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer second.Close()
		id, err = persistentRequest(second, "list")
		if err != nil {
			return err
		}
		return persistentReply(second, id, "There are 0 of a max of 5 players online: ")
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentWaitingCallerCancellationSendsNothing(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	address := persistentFixture(t, func(ln net.Listener) error {
		conn, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer conn.Close()
		id, err := persistentRequest(conn, "list")
		if err != nil {
			return err
		}
		close(entered)
		<-release
		if err := persistentReply(conn, id, "There are 0 of a max of 5 players online: "); err != nil {
			return err
		}
		id, err = persistentRequest(conn, "list")
		if err != nil {
			return err
		}
		if id != 4 {
			return errors.New("cancelled waiting call sent or consumed a request ID")
		}
		return persistentReply(conn, id, "There are 1 of a max of 5 players online: player")
	})
	c := NewPersistent(address, testPassword, 2*time.Second)
	defer c.Close()
	first := make(chan error, 1)
	go func() { _, err := c.Players(context.Background()); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request not received")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := c.Players(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting request: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	n, err := c.Players(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("following request: players=%d err=%v", n, err)
	}
}

func TestPersistentCancellationDuringReadInvalidatesConnection(t *testing.T) {
	entered := make(chan struct{})
	address := persistentFixture(t, func(ln net.Listener) error {
		first, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer first.Close()
		if _, err := persistentRequest(first, "list"); err != nil {
			return err
		}
		close(entered)
		if _, err := readPacket(first); !errors.Is(err, io.EOF) {
			return errors.New("cancelled exchange connection was not closed")
		}
		second, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer second.Close()
		id, err := persistentRequest(second, "list")
		if err != nil {
			return err
		}
		return persistentReply(second, id, "There are 0 of a max of 5 players online: ")
	})
	c := NewPersistent(address, testPassword, 2*time.Second)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Players(ctx); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not received")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not cancel promptly")
	}
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatalf("reconnect after cancellation: %v", err)
	}
}

func TestPersistentCompletedContextCannotCancelNextRequest(t *testing.T) {
	address := persistentFixture(t, func(ln net.Listener) error {
		conn, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			id, err := persistentRequest(conn, "list")
			if err != nil {
				return err
			}
			if err := persistentReply(conn, id, "There are 0 of a max of 5 players online: "); err != nil {
				return err
			}
		}
		return nil
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.Players(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatalf("old context interfered with reused connection: %v", err)
	}
}

func TestPersistentRejectsStaleReplyIDsAndReconnects(t *testing.T) {
	address := persistentFixture(t, func(ln net.Listener) error {
		first, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer first.Close()
		id, err := persistentRequest(first, "list")
		if err != nil {
			return err
		}
		if err := persistentReply(first, id, "There are 0 of a max of 5 players online: "); err != nil {
			return err
		}
		if _, err := persistentRequest(first, "list"); err != nil {
			return err
		}
		// A late duplicate of the previous command must not be accepted as
		// a current zero-player result.
		if err := writePacket(first, id, 0, "There are 0 of a max of 5 players online: "); err != nil {
			return err
		}
		if _, err := readPacket(first); !errors.Is(err, io.EOF) {
			return errors.New("stale reply did not invalidate connection")
		}
		second, err := acceptAuthenticated(ln)
		if err != nil {
			return err
		}
		defer second.Close()
		id, err = persistentRequest(second, "list")
		if err != nil {
			return err
		}
		return persistentReply(second, id, "There are 1 of a max of 5 players online: player")
	})
	c := NewPersistent(address, testPassword, time.Second)
	defer c.Close()
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Players(context.Background()); err == nil {
		t.Fatal("accepted stale player count")
	}
	n, err := c.Players(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("fresh connection: players=%d err=%v", n, err)
	}
}

type delayedDeadlineConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *delayedDeadlineConn) SetDeadline(deadline time.Time) error {
	if !deadline.IsZero() && deadline.Before(time.Now().Add(10*time.Millisecond)) {
		c.once.Do(func() { close(c.entered) })
		<-c.release
	}
	return nil
}

func TestContextCleanupJoinsRunningCancellationCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn := &delayedDeadlineConn{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(conn.release) })
	finish, err := bindContext(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-conn.entered:
	case <-time.After(time.Second):
		t.Fatal("cancellation callback did not run")
	}
	finished := make(chan struct{})
	go func() { finish(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("cleanup returned while old callback could still alter the connection")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(conn.release) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join completed callback")
	}
}
