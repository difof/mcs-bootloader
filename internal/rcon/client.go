// Package rcon provides bounded, authenticated Minecraft RCON requests. Clients
// can use single-command connections or an explicitly serialized persistent one.
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
	"sync/atomic"
	"time"
)

const (
	maxPacket         = 1 << 20
	maxResponse       = 1 << 20
	authID      int32 = 1
	commandID   int32 = 2
	boundaryID  int32 = 3
)

var (
	ErrAuthentication = errors.New("RCON authentication rejected")
	ErrClosed         = errors.New("RCON client is closed")
)

type Client struct {
	address  string
	password string
	timeout  atomic.Int64
	stats    *clientCounters
	// Only persistent clients use the fields below. gate protects their entire
	// exchange, connection lifecycle, request IDs, and closed flag.
	persistent bool
	gate       chan struct{}
	conn       net.Conn
	nextID     int32
	closed     bool
}

func New(address, password string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	c := &Client{address: address, password: password, stats: &clientCounters{}}
	c.timeout.Store(int64(timeout))
	return c
}

// Address is immutable for the lifetime of a client.
func (c *Client) Address() string { return c.address }

// SetTimeout changes the default for future requests without interrupting an
// exchange already in progress or reconnecting a healthy persistent session.
func (c *Client) SetTimeout(timeout time.Duration) {
	if timeout <= 0 {
		panic("RCON timeout must be positive")
	}
	c.timeout.Store(int64(timeout))
}

// NewPersistent reuses one authenticated connection for sequential requests.
// Every failed/uncertain exchange is returned without replay and invalidates the
// connection. A later caller can reconnect. Close releases the idle connection.
func NewPersistent(address, password string, timeout time.Duration) *Client {
	c := New(address, password, timeout)
	c.persistent = true
	c.gate = make(chan struct{}, 1)
	return c
}

// Close is idempotent. For a persistent client it waits for the current bounded
// exchange, closes its connection, and rejects future commands. Single-command
// clients have no idle resources to release.
func (c *Client) Close() error {
	if !c.persistent {
		return nil
	}
	c.gate <- struct{}{}
	defer func() { <-c.gate }()
	c.closed = true
	return c.discard()
}

// discard must be called with the persistent client's gate held.
func (c *Client) discard() error {
	conn := c.conn
	c.conn = nil
	if conn != nil {
		c.stats.connected.Add(-1)
		return conn.Close()
	}
	return nil
}

type packet struct {
	id   int32
	kind int32
	body []byte
}

func writePacket(w io.Writer, id, kind int32, text string) error {
	if len(text) > maxPacket-10 || strings.ContainsRune(text, 0) {
		return errors.New("RCON payload is invalid or too large")
	}
	buf := make([]byte, len(text)+14)
	binary.LittleEndian.PutUint32(buf, uint32(len(text)+10))
	binary.LittleEndian.PutUint32(buf[4:], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:], uint32(kind))
	copy(buf[12:], text)
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

func readPacket(r io.Reader) (packet, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return packet{}, err
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n < 10 || n > maxPacket {
		return packet{}, errors.New("RCON packet size outside bounds")
	}
	buf := make([]byte, int(n))
	if _, err := io.ReadFull(r, buf); err != nil {
		return packet{}, err
	}
	if buf[len(buf)-1] != 0 || buf[len(buf)-2] != 0 {
		return packet{}, errors.New("RCON packet missing terminators")
	}
	if bytes.IndexByte(buf[8:len(buf)-2], 0) >= 0 {
		return packet{}, errors.New("RCON response contains embedded terminator")
	}
	return packet{id: int32(binary.LittleEndian.Uint32(buf)), kind: int32(binary.LittleEndian.Uint32(buf[4:])), body: buf[8 : len(buf)-2]}, nil
}

// Command executes one command and collects all response fragments. A distinct
// type-0 request is a response boundary: Minecraft answers unknown RCON request
// types after finishing the preceding command, while Source echoes type 0.
// It is sent only after the first command response proves that Minecraft has
// consumed the command. Some Minecraft RCON readers mishandle pipelined frames.
// This avoids timing guesses and never interprets a partial list as zero players.
// Its response is consumed by ID and never reaches the caller or logs.
func (c *Client) Command(ctx context.Context, command string) (string, error) {
	return c.CommandWithTimeout(ctx, command, time.Duration(c.timeout.Load()))
}

// CommandWithTimeout uses the same serialized connection with a per-operation
// deadline. Operator commands can take longer than a routine health check.
func (c *Client) CommandWithTimeout(ctx context.Context, command string, timeout time.Duration) (response string, resultErr error) {
	started := time.Now()
	var contextErr error
	c.stats.inFlight.Add(1)
	defer c.stats.inFlight.Add(-1)
	defer func() { c.observe(command, started, response, resultErr, contextErr) }()
	if c.password == "" {
		return "", errors.New("RCON password is empty")
	}
	if command == "" || strings.ContainsAny(command, "\x00\r\n") {
		return "", errors.New("invalid RCON command")
	}
	if timeout <= 0 {
		return "", errors.New("RCON timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Capture the caller/deadline outcome before our deferred cleanup cancels
	// an otherwise healthy context; socket cancellation surfaces as a timeout.
	defer func() { contextErr = ctx.Err() }()
	if c.persistent {
		return c.persistentCommand(ctx, command)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return "", fmt.Errorf("RCON connect: %w", err)
	}
	defer conn.Close()
	defer c.stats.connected.Add(-1)
	finish, err := bindContext(ctx, conn)
	if err != nil {
		return "", err
	}
	defer finish()
	return c.exchange(conn, command)
}

// bindContext joins a cancellation callback before returning the connection to
// an idle pool. An old callback must never alter a later request's deadline.
func bindContext(ctx context.Context, conn net.Conn) (func(), error) {
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}, nil
}

func (c *Client) persistentCommand(ctx context.Context, command string) (response string, err error) {
	c.stats.queued.Add(1)
	select {
	case <-ctx.Done():
		c.stats.queued.Add(-1)
		return "", ctx.Err()
	case c.gate <- struct{}{}:
		c.stats.queued.Add(-1)
	}
	defer func() { <-c.gate }()
	if c.closed {
		return "", ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Rotate a connection before request IDs could overflow or be reused.
	if c.conn != nil && c.nextID > 1<<31-3 {
		_ = c.discard()
	}
	fresh := c.conn == nil
	if fresh {
		c.conn, err = c.dial(ctx)
		if err != nil {
			return "", fmt.Errorf("RCON connect: %w", err)
		}
		c.nextID = commandID
	}
	conn := c.conn
	finish, err := bindContext(ctx, conn)
	if err != nil {
		_ = c.discard()
		return "", err
	}
	defer func() {
		finish()
		if err != nil || ctx.Err() != nil || command == "stop" {
			_ = c.discard()
		} else if err := conn.SetDeadline(time.Time{}); err != nil {
			// The response is already complete, but this socket is no longer
			// safe to reuse. Do not turn success into a command replay.
			_ = c.discard()
		}
	}()
	if fresh {
		if err := c.authenticate(conn); err != nil {
			return "", err
		}
	}
	id := c.nextID
	c.nextID += 2
	return c.request(conn, command, id, id+1)
}

// exchange keeps protocol sequencing separate from connection/deadline setup.
func (c *Client) exchange(conn io.ReadWriter, command string) (string, error) {
	if err := c.authenticate(conn); err != nil {
		return "", err
	}
	return c.request(conn, command, commandID, boundaryID)
}

func (c *Client) authenticate(conn io.ReadWriter) (resultErr error) {
	c.stats.authenticationAttempts.Add(1)
	defer func() {
		if resultErr != nil {
			c.stats.authenticationErrors.Add(1)
		} else {
			c.stats.authenticationSuccesses.Add(1)
		}
	}()
	if err := writePacket(conn, authID, 3, c.password); err != nil {
		return fmt.Errorf("RCON authentication write: %w", err)
	}
	authenticated := false
	for i := 0; i < 4; i++ {
		p, err := readPacket(conn)
		if err != nil {
			return fmt.Errorf("RCON authentication response: %w", err)
		}
		if p.id == -1 {
			return ErrAuthentication
		}
		if p.id != authID {
			return errors.New("RCON authentication response ID mismatch")
		}
		if p.kind == 2 {
			authenticated = true
			break
		}
		if p.kind != 0 || len(p.body) != 0 {
			return errors.New("invalid RCON authentication response")
		}
	}
	if !authenticated {
		return errors.New("RCON authentication response limit exceeded")
	}
	return nil
}

func (c *Client) request(conn io.ReadWriter, command string, requestID, endID int32) (string, error) {
	if err := writePacket(conn, requestID, 2, command); err != nil {
		return "", fmt.Errorf("RCON command write: %w", err)
	}
	var response strings.Builder
	seen := false
	boundarySent := false
	for i := 0; i < 1024; i++ {
		p, err := readPacket(conn)
		if err != nil {
			// A stopping Minecraft server can close RCON immediately after its
			// affirmative reply, before it reads the boundary request.
			if command == "stop" && seen && isStopReply(response.String()) {
				return response.String(), nil
			}
			return "", fmt.Errorf("RCON command response incomplete: %w", err)
		}
		if p.id == -1 {
			return "", ErrAuthentication
		}
		if p.kind != 0 {
			return "", errors.New("invalid RCON command response type")
		}
		if p.id == endID {
			if !seen {
				return "", errors.New("RCON command response missing")
			}
			return response.String(), nil
		}
		if p.id != requestID {
			return "", errors.New("RCON command response ID mismatch")
		}
		if len(p.body) > maxResponse-response.Len() {
			return "", errors.New("RCON response too large")
		}
		seen = true
		response.Write(p.body)
		if !boundarySent {
			// Wait for a command response before sending another request so
			// the two frames cannot coalesce in Minecraft's socket read.
			if err := writePacket(conn, endID, 0, ""); err != nil {
				if command == "stop" && isStopReply(response.String()) {
					return response.String(), nil
				}
				return "", fmt.Errorf("RCON boundary write: %w", err)
			}
			boundarySent = true
		}
	}
	return "", errors.New("RCON response packet limit exceeded")
}
