// Package mcproto implements only the bounded Minecraft handshake, status, and
// pre-login messages needed by the gateway. Gameplay is deliberately opaque.
package mcproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxHandshake  = 65536
	MaxLoginStart = 4096
	MaxStatus     = 1 << 20
)

// Handshake retains the original frame. Host may contain opaque NUL-separated
// Forge extensions and must never replace Raw when forwarding to the backend.
type Handshake struct {
	Raw       []byte
	Protocol  int
	Host      string
	Port      uint16
	NextState int
}

// AppendVarInt appends Minecraft's signed, non-ZigZag 32-bit VarInt encoding.
func AppendVarInt(dst []byte, n int32) []byte {
	u := uint32(n)
	for u >= 0x80 {
		dst = append(dst, byte(u)|0x80)
		u >>= 7
	}
	return append(dst, byte(u))
}

// EncodePacket frames a payload (which already includes its packet ID).
func EncodePacket(payload []byte) []byte {
	result := AppendVarInt(make([]byte, 0, len(payload)+5), int32(len(payload)))
	return append(result, payload...)
}

func readVarInt(r io.Reader) (int32, []byte, error) {
	var raw [5]byte
	var value uint32
	for i := range raw {
		if _, err := io.ReadFull(r, raw[i:i+1]); err != nil {
			return 0, nil, err
		}
		b := raw[i]
		if i == 4 && b&0xf0 != 0 {
			return 0, nil, errors.New("invalid 32-bit VarInt")
		}
		value |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int32(value), append([]byte(nil), raw[:i+1]...), nil
		}
	}
	return 0, nil, errors.New("unterminated VarInt")
}

// ReadPacket reads exactly one frame with no read-ahead and retains the original
// length prefix, including valid noncanonical encodings.
func ReadPacket(r io.Reader, max int) (raw []byte, payload []byte, err error) {
	n, prefix, err := readVarInt(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read packet length: %w", err)
	}
	if n <= 0 || max <= 0 || int64(n) > int64(max) {
		return nil, nil, errors.New("packet length outside allowed bounds")
	}
	raw = make([]byte, len(prefix)+int(n))
	copy(raw, prefix)
	payload = raw[len(prefix):]
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, nil, fmt.Errorf("read packet: %w", err)
	}
	return raw, payload, nil
}

func readString(r *bytes.Reader, max int) (string, error) {
	n, _, err := readVarInt(r)
	if err != nil || n < 0 || int64(n) > int64(max) || int64(n) > int64(r.Len()) {
		return "", errors.New("invalid string length")
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("invalid UTF-8 string")
	}
	return string(b), nil
}

func ReadHandshake(r io.Reader) (Handshake, error) {
	raw, payload, err := ReadPacket(r, MaxHandshake)
	if err != nil {
		return Handshake{}, err
	}
	b := bytes.NewReader(payload)
	id, _, err := readVarInt(b)
	if err != nil || id != 0 {
		return Handshake{}, errors.New("expected handshake packet")
	}
	protocol, _, err := readVarInt(b)
	if err != nil {
		return Handshake{}, errors.New("invalid handshake protocol")
	}
	host, err := readString(b, 32767)
	if err != nil || host == "" {
		return Handshake{}, errors.New("invalid handshake hostname")
	}
	var port uint16
	if err := binary.Read(b, binary.BigEndian, &port); err != nil {
		return Handshake{}, errors.New("invalid handshake port")
	}
	state, _, err := readVarInt(b)
	if err != nil || (state != 1 && state != 2 && state != 3) || b.Len() != 0 {
		return Handshake{}, errors.New("invalid handshake state")
	}
	return Handshake{Raw: raw, Protocol: int(protocol), Host: host, Port: port, NextState: int(state)}, nil
}

// ReadLoginStart validates the modern (1.20.2+) login-start envelope while
// preserving every byte. It does not interpret or authenticate the UUID.
func ReadLoginStart(r io.Reader, protocol int) ([]byte, string, error) {
	if protocol < 764 {
		return nil, "", errors.New("unsupported login-start protocol")
	}
	raw, payload, err := ReadPacket(r, MaxLoginStart)
	if err != nil {
		return nil, "", err
	}
	b := bytes.NewReader(payload)
	id, _, err := readVarInt(b)
	if err != nil || id != 0 {
		return nil, "", errors.New("expected login-start packet")
	}
	name, err := readString(b, 64)
	if err != nil || name == "" || utf8.RuneCountInString(name) > 16 {
		return nil, "", errors.New("invalid login name")
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, "", errors.New("invalid login name")
		}
	}
	if b.Len() != 16 {
		return nil, "", errors.New("login-start UUID missing or trailing fields present")
	}
	return raw, name, nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func validStatus(status json.RawMessage) bool {
	b := bytes.TrimSpace(status)
	return len(b) > 0 && len(b) <= MaxStatus && b[0] == '{' && json.Valid(b)
}

func WriteStatus(w io.Writer, status json.RawMessage) error {
	if !validStatus(status) {
		return errors.New("invalid or oversized status JSON")
	}
	payload := AppendVarInt([]byte{0}, int32(len(status)))
	payload = append(payload, status...)
	return writeAll(w, EncodePacket(payload))
}

// HandleStatus consumes the status request after a handshake and optionally
// echoes its ping. The caller must set a bounded deadline on conn.
func HandleStatus(conn net.Conn, status json.RawMessage) error {
	_, payload, err := ReadPacket(conn, 16)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, []byte{0}) {
		return errors.New("expected empty status request")
	}
	if err := WriteStatus(conn, status); err != nil {
		return err
	}
	raw, payload, err := ReadPacket(conn, 16)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		var neterr net.Error
		if errors.As(err, &neterr) && neterr.Timeout() {
			return nil
		}
		return err
	}
	if len(payload) != 9 || payload[0] != 1 {
		return errors.New("invalid status ping")
	}
	return writeAll(conn, raw)
}

// WriteDisconnect sends a pre-login disconnect. In Minecraft 1.21.1 this is
// packet 0 containing a JSON string, unlike configuration/game disconnect NBT.
// Schema: PrismarineJS/minecraft-data data/pc/1.21.1/proto.yml, login.toClient.
func WriteDisconnect(w io.Writer, protocol int, message string) error {
	if protocol < 4 {
		return errors.New("unsupported disconnect protocol")
	}
	if len(message) > 16384 {
		return errors.New("disconnect message too long")
	}
	b, err := json.Marshal(struct {
		Text string `json:"text"`
	}{message})
	if err != nil {
		return err
	}
	payload := AppendVarInt([]byte{0}, int32(len(b)))
	payload = append(payload, b...)
	return writeAll(w, EncodePacket(payload))
}

// QueryStatus fetches the complete backend JSON without re-encoding or dropping
// unknown mod extension fields. Its lifetime is bounded even without a deadline.
func QueryStatus(ctx context.Context, address string, protocol int) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("status address: %w", err)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil {
		return nil, errors.New("invalid status port")
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("status connect: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	payload := AppendVarInt([]byte{0}, int32(protocol))
	payload = AppendVarInt(payload, int32(len(host)))
	payload = append(payload, host...)
	payload = binary.BigEndian.AppendUint16(payload, uint16(port))
	payload = AppendVarInt(payload, 1)
	if err := writeAll(conn, append(EncodePacket(payload), 1, 0)); err != nil {
		return nil, err
	}
	_, payload, err = ReadPacket(conn, MaxStatus+6)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(payload)
	id, _, err := readVarInt(r)
	if err != nil || id != 0 {
		return nil, errors.New("expected status response")
	}
	text, err := readString(r, MaxStatus)
	if err != nil || r.Len() != 0 || !validStatus(json.RawMessage(text)) {
		return nil, errors.New("invalid status response JSON")
	}
	return json.RawMessage(text), nil
}
