package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCommandRejectsInvalidInputBeforeReadingConfiguration(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "must-not-be-read.json")
	cases := [][]string{
		nil, {""}, {"   "}, {"/"}, {"/   "},
		{"say", "hello\nstop"}, {"say", "hello\rstop"},
		{"say", "hello\x00stop"}, {"\nlist"},
	}
	for _, input := range cases {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			var output bytes.Buffer
			args := append([]string{"--config", missingConfig}, input...)
			err := runCommand(args, &output)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected command validation before configuration read, got %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("invalid command produced output: %q", output.String())
			}
		})
	}
	if err := runCommand([]string{"--config", missingConfig, "--timeout=0s", "list"}, io.Discard); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid timeout must be rejected before configuration read: %v", err)
	}
}

func TestRunCommandSendsLiteralArgumentsOnceAndPrintsReply(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "shell-must-not-run")
	jsonText := `{"text":"literal \"quotes\", spaces, $(touch ` + sentinel + `), and ` + "`touch " + sentinel + "`" + `;"}`
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"operator", []string{"op", "ExamplePlayer"}, "op ExamplePlayer"},
		{"leading slash", []string{"/op", "ExamplePlayer"}, "op ExamplePlayer"},
		{"quoted JSON and shell text", []string{"tellraw", "@a", jsonText}, "tellraw @a " + jsonText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands atomic.Int32
			const reply = "Server reply: command completed\nsecond line"
			address, done := commandTestServer(t, func(conn net.Conn) error {
				if err := commandTestAuthenticate(conn); err != nil {
					return err
				}
				p, err := readCommandTestPacket(conn)
				if err != nil {
					return err
				}
				commands.Add(1)
				if p.kind != 2 || p.body != tc.want {
					return fmt.Errorf("unexpected command: kind=%d body=%q", p.kind, p.body)
				}
				if err := writeCommandTestPacket(conn, p.id, 0, reply); err != nil {
					return err
				}
				boundary, err := readCommandTestPacket(conn)
				if err != nil {
					return err
				}
				if boundary.kind != 0 || boundary.id == p.id || boundary.body != "" {
					return errors.New("expected a distinct response boundary")
				}
				if err := writeCommandTestPacket(conn, boundary.id, 0, "Unknown request 0"); err != nil {
					return err
				}
				if extra, err := readCommandTestPacket(conn); !errors.Is(err, io.EOF) {
					return fmt.Errorf("expected client close after one command, got %+v, %v", extra, err)
				}
				return nil
			})
			config := commandTestConfig(t, address)
			var output bytes.Buffer
			args := append([]string{"--config", config, "--timeout=2s", "--"}, tc.args...)
			if err := runCommand(args, &output); err != nil {
				t.Fatal(err)
			}
			commandTestWait(t, done)
			if commands.Load() != 1 || output.String() != reply+"\n" {
				t.Fatalf("commands=%d reply=%q", commands.Load(), output.String())
			}
		})
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command arguments were executed by a shell: %v", err)
	}
}

func TestRunCommandLostReplyIsNotRetried(t *testing.T) {
	var commands atomic.Int32
	address, done := commandTestServer(t, func(conn net.Conn) error {
		if err := commandTestAuthenticate(conn); err != nil {
			return err
		}
		p, err := readCommandTestPacket(conn)
		if err != nil {
			return err
		}
		commands.Add(1)
		if p.kind != 2 || p.body != "give ExamplePlayer minecraft:stone 1" {
			return fmt.Errorf("unexpected command: %+v", p)
		}
		// The command may have executed. Close without replying; the caller must
		// report uncertainty, rather than automatically duplicating its effect.
		return nil
	})
	var output bytes.Buffer
	err := runCommand([]string{"--config", commandTestConfig(t, address), "--timeout=500ms", "give", "ExamplePlayer", "minecraft:stone", "1"}, &output)
	commandTestWait(t, done)
	if err == nil || !strings.Contains(err.Error(), "may already have executed") || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("missing uncertain-delivery error: %v", err)
	}
	if commands.Load() != 1 || output.Len() != 0 {
		t.Fatalf("commands=%d output=%q", commands.Load(), output.String())
	}
}

func TestRunCommandAuthenticationFailureDoesNotExposePassword(t *testing.T) {
	address, done := commandTestServer(t, func(conn net.Conn) error {
		p, err := readCommandTestPacket(conn)
		if err != nil {
			return err
		}
		if p.kind != 3 || p.body != commandTestPassword {
			return errors.New("expected authentication from the configured password file")
		}
		return writeCommandTestPacket(conn, -1, 2, "")
	})
	var output bytes.Buffer
	err := runCommand([]string{"--config", commandTestConfig(t, address), "list"}, &output)
	commandTestWait(t, done)
	if err == nil || !strings.Contains(err.Error(), "authentication rejected") {
		t.Fatalf("unexpected authentication error: %v", err)
	}
	if strings.Contains(err.Error()+output.String(), commandTestPassword) || output.Len() != 0 {
		t.Fatal("authentication failure leaked password or output")
	}
}

const commandTestPassword = "operator-command-test-secret"

func commandTestConfig(t *testing.T, address string) string {
	t.Helper()
	dir := t.TempDir()
	passwordPath := filepath.Join(dir, "rcon.password")
	if err := os.WriteFile(passwordPath, []byte(commandTestPassword+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"rcon_address": address, "rcon_password_file": passwordPath})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func commandTestServer(t *testing.T, handle func(net.Conn) error) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		// A second attempted connection cannot be serviced. Commands must not
		// hide lost replies behind a retry against the same server.
		listener.Close()
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			done <- err
			return
		}
		done <- handle(conn)
	}()
	return listener.Addr().String(), done
}

func commandTestWait(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("fake RCON server did not finish")
	}
}

type commandTestPacket struct {
	id, kind int32
	body     string
}

func readCommandTestPacket(r io.Reader) (commandTestPacket, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return commandTestPacket{}, err
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n < 10 || n > 4096 {
		return commandTestPacket{}, fmt.Errorf("invalid test RCON packet size %d", n)
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return commandTestPacket{}, err
	}
	if body[n-1] != 0 || body[n-2] != 0 {
		return commandTestPacket{}, errors.New("invalid test RCON terminators")
	}
	return commandTestPacket{int32(binary.LittleEndian.Uint32(body)), int32(binary.LittleEndian.Uint32(body[4:])), string(body[8 : n-2])}, nil
}

func writeCommandTestPacket(w io.Writer, id, kind int32, body string) error {
	raw := make([]byte, len(body)+14)
	binary.LittleEndian.PutUint32(raw, uint32(len(body)+10))
	binary.LittleEndian.PutUint32(raw[4:], uint32(id))
	binary.LittleEndian.PutUint32(raw[8:], uint32(kind))
	copy(raw[12:], body)
	_, err := io.Copy(w, bytes.NewReader(raw))
	return err
}

func commandTestAuthenticate(conn net.Conn) error {
	p, err := readCommandTestPacket(conn)
	if err != nil {
		return err
	}
	if p.kind != 3 || p.body != commandTestPassword {
		return errors.New("expected authentication from the configured password file")
	}
	return writeCommandTestPacket(conn, p.id, 2, "")
}
