package mcproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

const MaxHandshake = 65536

// Handshake retains the original frame. Host can contain opaque NUL-separated
// Forge extensions and must never replace Raw when forwarding to the backend.
type Handshake struct {
	Raw       []byte
	Protocol  int
	Host      string
	Port      uint16
	NextState int
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
	if err != nil || !validNextState(int(protocol), int(state)) || b.Len() != 0 {
		return Handshake{}, errors.New("invalid handshake state")
	}
	return Handshake{Raw: raw, Protocol: int(protocol), Host: host, Port: port, NextState: int(state)}, nil
}
