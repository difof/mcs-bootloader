package mcproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
)

const MaxStatus = 1 << 20

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

// HandleStatus consumes a status request and optionally echoes its ping. The
// caller must set a bounded deadline on conn.
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
