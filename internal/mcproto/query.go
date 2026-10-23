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
)

// QueryStatus fetches the complete backend JSON without dropping unknown mod
// metadata. Its lifetime is bounded even without a deadline from the caller.
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
	if err := writeStatusRequest(conn, host, uint16(port), protocol); err != nil {
		return nil, err
	}
	return readStatusResponse(conn)
}

func writeStatusRequest(w io.Writer, host string, port uint16, protocol int) error {
	payload := AppendVarInt([]byte{0}, int32(protocol))
	payload = AppendVarInt(payload, int32(len(host)))
	payload = append(payload, host...)
	payload = binary.BigEndian.AppendUint16(payload, port)
	payload = AppendVarInt(payload, 1)
	return writeAll(w, append(EncodePacket(payload), 1, 0))
}

func readStatusResponse(reader io.Reader) (json.RawMessage, error) {
	_, payload, err := ReadPacket(reader, MaxStatus+6)
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
