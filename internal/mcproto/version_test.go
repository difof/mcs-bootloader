package mcproto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// Independent wire fixtures for the five schema eras. Key/signature/UUID bytes
// are deliberately opaque: this gateway validates framing, not authentication.
func TestLoginLayoutsPreserveBytes(t *testing.T) {
	uuid := bytes.Repeat([]byte{0xa5}, 16)
	signed := binary.BigEndian.AppendUint64([]byte{1}, 1700000000000)
	signed = append(signed, 3, 0xab, 0xcd, 0xef, 2, 0xfe, 0xff)
	cases := []struct {
		protocol int
		tail     []byte
	}{
		{5, nil}, {340, nil}, {758, nil},
		{759, []byte{0}}, {759, signed},
		{760, []byte{0, 0}}, {760, append(append([]byte{}, signed...), append([]byte{1}, uuid...)...)},
		{761, []byte{0}}, {763, append([]byte{1}, uuid...)},
		{764, uuid}, {767, uuid}, {775, uuid},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d/%d", tc.protocol, i), func(t *testing.T) {
			body := append([]byte{0, 6, 'P', 'l', 'a', 'y', 'e', 'r'}, tc.tail...)
			frame := EncodePacket(body)
			frame = append([]byte{frame[0] | 0x80, 0}, frame[1:]...)
			next := []byte{0xff, 0xfe, 0, 0x80}
			r := bytes.NewReader(append(append([]byte{}, frame...), next...))
			got, name, err := ReadLoginStart(byteReader{r}, tc.protocol)
			if err != nil || name != "Player" || !bytes.Equal(got, frame) || r.Len() != len(next) {
				t.Fatalf("login changed/read ahead: name=%q remaining=%d err=%v", name, r.Len(), err)
			}
		})
	}
}

func TestLoginLayoutRejectsIncompleteAndTrailingFields(t *testing.T) {
	cases := []struct {
		protocol int
		tail     []byte
	}{
		{5, []byte{0}},                  // legacy has no fields after the name
		{759, nil}, {759, []byte{1, 0}}, // missing signature flag/timestamp
		{759, append(make([]byte, 9), 0)}, // absent signature plus trailing bytes
		{760, []byte{0}},                  // missing UUID presence flag
		{761, nil}, {763, []byte{1, 0}},   // missing flag/truncated UUID
		{764, bytes.Repeat([]byte{1}, 15)}, {775, bytes.Repeat([]byte{1}, 17)},
	}
	// A signed key length must stay inside its already bounded packet.
	oversized := append([]byte{1}, make([]byte, 8)...)
	oversized = AppendVarInt(oversized, MaxLoginStart)
	cases = append(cases, struct {
		protocol int
		tail     []byte
	}{759, oversized})
	for _, tc := range cases {
		body := append([]byte{0, 1, 'P'}, tc.tail...)
		if _, _, err := ReadLoginStart(bytes.NewReader(EncodePacket(body)), tc.protocol); err == nil {
			t.Fatalf("accepted protocol %d malformed fields %x", tc.protocol, tc.tail)
		}
	}
}

func TestVersionBoundsAndTransferState(t *testing.T) {
	for _, protocol := range []int{0, 4, 6, 48, 776, 1 << 30} {
		if SupportedProtocol(protocol) {
			t.Fatalf("unverified protocol %d supported", protocol)
		}
		if _, _, err := ReadLoginStart(bytes.NewReader(loginFrame("Player")), protocol); err == nil {
			t.Fatalf("unverified protocol %d accepted", protocol)
		}
	}
	for _, tc := range []struct {
		protocol int32
		allowed  bool
	}{{5, false}, {765, false}, {766, true}, {775, true}, {776, false}} {
		_, err := ReadHandshake(bytes.NewReader(handshakeForProtocol("host", 3, tc.protocol)))
		if (err == nil) != tc.allowed {
			t.Fatalf("protocol %d transfer allowed=%v err=%v", tc.protocol, tc.allowed, err)
		}
	}
}
