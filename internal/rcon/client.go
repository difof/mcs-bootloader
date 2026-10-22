// Package rcon provides bounded, authenticated Minecraft RCON requests. A new
// connection per command avoids stale authentication and shared-stream races.
package rcon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxPacket         = 1 << 20
	maxResponse       = 1 << 20
	authID      int32 = 1
	commandID   int32 = 2
	boundaryID  int32 = 3
)

var ErrAuthentication = errors.New("RCON authentication rejected")

type Client struct {
	address  string
	password string
	timeout  time.Duration
}

func New(address, password string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{address: address, password: password, timeout: timeout}
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
	if c.password == "" {
		return "", errors.New("RCON password is empty")
	}
	if command == "" || strings.ContainsAny(command, "\x00\r\n") {
		return "", errors.New("invalid RCON command")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return "", fmt.Errorf("RCON connect: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	return c.exchange(conn, command)
}

// exchange keeps protocol sequencing separate from connection/deadline setup.
func (c *Client) exchange(conn io.ReadWriter, command string) (string, error) {
	if err := writePacket(conn, authID, 3, c.password); err != nil {
		return "", fmt.Errorf("RCON authentication write: %w", err)
	}
	authenticated := false
	for i := 0; i < 4; i++ {
		p, err := readPacket(conn)
		if err != nil {
			return "", fmt.Errorf("RCON authentication response: %w", err)
		}
		if p.id == -1 {
			return "", ErrAuthentication
		}
		if p.id != authID {
			return "", errors.New("RCON authentication response ID mismatch")
		}
		if p.kind == 2 {
			authenticated = true
			break
		}
		if p.kind != 0 || len(p.body) != 0 {
			return "", errors.New("invalid RCON authentication response")
		}
	}
	if !authenticated {
		return "", errors.New("RCON authentication response limit exceeded")
	}
	if err := writePacket(conn, commandID, 2, command); err != nil {
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
		if p.id == boundaryID {
			if !seen {
				return "", errors.New("RCON command response missing")
			}
			return response.String(), nil
		}
		if p.id != commandID {
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
			if err := writePacket(conn, boundaryID, 0, ""); err != nil {
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

var playerList = regexp.MustCompile(`^There are ([0-9]+) of a max of ([0-9]+) players online:\s*(.*)$`)
var formatting = regexp.MustCompile(`(?i)§[0-9a-fk-or]`)

func parsePlayers(response string) (int, error) {
	response = strings.TrimSpace(formatting.ReplaceAllString(response, ""))
	parts := playerList.FindStringSubmatch(response)
	if len(parts) != 4 {
		return 0, errors.New("unrecognized RCON player-list response")
	}
	count, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, errors.New("invalid RCON player count")
	}
	max, err := strconv.Atoi(parts[2])
	if err != nil || max < 0 || count < 0 {
		return 0, errors.New("invalid RCON player capacity")
	}
	// A supposedly empty list with any unexpected trailing text is ambiguous.
	if count == 0 && strings.TrimSpace(parts[3]) != "" {
		return 0, errors.New("contradictory RCON empty-player response")
	}
	if count > 0 && strings.TrimSpace(parts[3]) == "" {
		return 0, errors.New("RCON player names missing from nonempty list")
	}
	return count, nil
}

func (c *Client) Players(ctx context.Context) (int, error) {
	response, err := c.Command(ctx, "list")
	if err != nil {
		return 0, err
	}
	return parsePlayers(response)
}

func isStopReply(response string) bool {
	response = strings.TrimSpace(formatting.ReplaceAllString(response, ""))
	return response == "Stopping the server" || response == "Stopping server"
}

func (c *Client) Stop(ctx context.Context) error {
	response, err := c.Command(ctx, "stop")
	if err != nil {
		return err
	}
	if !isStopReply(response) {
		return errors.New("RCON stop was not acknowledged")
	}
	return nil
}
