package mcproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"
)

const MaxLoginStart = 4096

// ReadLoginStart validates the version's complete login-start envelope without
// authenticating its name, UUID or key material. It retains the original frame
// and never consumes any later mod/authentication packet.
func ReadLoginStart(r io.Reader, protocol int) ([]byte, string, error) {
	profile, supported := profileFor(protocol)
	if !supported {
		return nil, "", fmt.Errorf("unsupported login-start protocol %d; supported: %s", protocol, SupportedVersions)
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
	name, err := readLoginName(b)
	if err != nil {
		return nil, "", err
	}
	if err := profile.readLoginFields(b); err != nil || b.Len() != 0 {
		return nil, "", errors.New("incomplete login-start envelope or unexpected trailing fields")
	}
	return raw, name, nil
}

func readLoginName(r *bytes.Reader) (string, error) {
	name, err := readString(r, 64)
	if err != nil || name == "" || utf8.RuneCountInString(name) > 16 {
		return "", errors.New("invalid login name")
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("invalid login name")
		}
	}
	return name, nil
}

func (p versionProfile) readLoginFields(r *bytes.Reader) error {
	if p.login == loginSigned || p.login == loginSignedUUID {
		if err := readOptionalSignature(r); err != nil {
			return err
		}
	}
	if p.login == loginSignedUUID || p.login == loginOptionalUUID {
		present, err := readBool(r)
		if err != nil || !present {
			return err
		}
		return skipBytes(r, 16)
	}
	if p.login == loginUUID {
		return skipBytes(r, 16)
	}
	return nil
}

func readOptionalSignature(r *bytes.Reader) error {
	present, err := readBool(r)
	if err != nil || !present {
		return err
	}
	if err := skipBytes(r, 8); err != nil { // signature expiry timestamp
		return err
	}
	for i := 0; i < 2; i++ { // opaque public key and signature, each VarInt-sized
		length, _, err := readVarInt(r)
		if err != nil {
			return err
		}
		if err := skipBytes(r, int(length)); err != nil {
			return err
		}
	}
	return nil
}

func readBool(r *bytes.Reader) (bool, error) {
	b, err := r.ReadByte()
	return b != 0, err
}

func skipBytes(r *bytes.Reader, length int) error {
	if length < 0 || length > r.Len() {
		return io.ErrUnexpectedEOF
	}
	_, err := r.Seek(int64(length), io.SeekCurrent)
	return err
}

// WriteDisconnect sends a pre-login JSON component (packet 0) for each supported
// version. Later configuration/game disconnect formats are not used here.
func WriteDisconnect(w io.Writer, protocol int, message string) error {
	if !SupportedProtocol(protocol) {
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
