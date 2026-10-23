package mcproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// AppendVarInt appends Minecraft's signed, non-ZigZag 32-bit VarInt encoding.
func AppendVarInt(dst []byte, n int32) []byte {
	u := uint32(n)
	for u >= 0x80 {
		dst = append(dst, byte(u)|0x80)
		u >>= 7
	}
	return append(dst, byte(u))
}

// EncodePacket frames a payload that already includes its packet ID.
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

// ReadPacket reads one bounded frame with no read-ahead. It retains the original
// length prefix, including valid noncanonical VarInt encodings.
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
