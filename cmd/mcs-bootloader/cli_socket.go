package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/difof/mcs-bootloader/internal/backend"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func socketIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) && os.Geteuid() != 0 {
			return "root", nil
		}
		return "", fmt.Errorf("gateway control socket is unavailable: %w; use gateway start if it is stopped", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("control path is not a Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot inspect control socket owner")
	}
	if os.Geteuid() == 0 || uint32(os.Geteuid()) == stat.Uid {
		return "", nil
	}
	// The operator executable may live under a private home directory that
	// the service account cannot traverse. Root connects to the same private
	// socket without requiring a second installed client or changing its mode.
	return "root", nil
}

var socketCommands = map[string]string{
	"status": "status", "stats": "metrics", "metrics": "metrics",
	"start": "wake", "wake": "wake", "stop": "sleep", "sleep": "sleep",
	"config live": "config", "reload": "reload", "config reload": "reload",
	"restart-check": "restart-check", "command": "command",
}

type socketTarget struct {
	Path, Socket string
	Values       map[string]string
	readAsRoot   bool
}

func runSocketOperation(ctx context.Context, o cliOptions, out *cliOutput, original []string) error {
	command := socketCommands[o.Command]
	if command == "" {
		return fmt.Errorf("unsupported operation %s", o.Command)
	}
	target, err := resolveSocketTarget(o, command)
	if target.readAsRoot {
		return reexecAs(ctx, "root", withEnvArgument(original, target.Path), out)
	}
	if err != nil {
		return err
	}
	if command == "command" && o.Direct {
		return runDirectCommand(o, target, out)
	}
	user, err := socketIdentity(target.Socket)
	if err != nil {
		return err
	}
	if user != "" {
		return reexecAs(ctx, user, socketForwardedArgs(o, command, target), out)
	}
	call, err := prepareSocketCall(o, command, target)
	if err != nil {
		return err
	}
	raw, err := call.send(ctx, target.Socket, command)
	if err != nil {
		return err
	}
	return renderSocketReply(o, command, raw, out)
}

func resolveSocketTarget(o cliOptions, command string) (socketTarget, error) {
	target := socketTarget{Socket: o.Socket}
	validateConfig := command == "reload" || command == "restart-check" || o.Direct
	if o.Env != "" || target.Socket == "" || validateConfig {
		var err error
		target.Path, err = resolveEnv(o.Env, false)
		if err != nil {
			return target, err
		}
		if validateConfig {
			cfg, err := backend.LoadConfig(target.Path)
			if err != nil {
				return target, err
			}
			if target.Socket == "" {
				target.Socket = cfg.AdminSocket
			}
		} else {
			target.Values, err = readOperatorValues(target.Path)
			if err != nil {
				target.readAsRoot = errors.Is(err, os.ErrPermission) && os.Geteuid() != 0
				return target, err
			}
			if target.Socket == "" {
				target.Socket = target.Values["MC_ADMIN_SOCKET"]
				if target.Socket == "" {
					target.Socket = backend.DefaultConfig().AdminSocket
				}
			}
		}
	}
	if target.Socket != "" && (!filepath.IsAbs(target.Socket) || filepath.Clean(target.Socket) == "/" || strings.ContainsAny(target.Socket, "\x00\r\n")) {
		return target, errors.New("admin socket must be an absolute non-root path without control characters")
	}
	return target, nil
}

func runDirectCommand(o cliOptions, target socketTarget, out *cliOutput) error {
	if raw, err := readBoundedFile(target.Path, 65536); err == nil && !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		target.Values, err = backend.OperatorValues(raw)
		if err != nil {
			return err
		}
	}
	duration, err := validatedCommandTimeout(o.Timeout, target.Values)
	if err != nil {
		return err
	}
	var reply strings.Builder
	args := append([]string{"--config", target.Path, "--timeout", duration.String(), "--"}, o.Args...)
	if err := runCommand(args, &reply); err != nil {
		return err
	}
	return out.CommandReply(strings.TrimSuffix(reply.String(), "\n"))
}

func socketForwardedArgs(o cliOptions, command string, target socketTarget) []string {
	// Pin the selected socket before root re-exec so diagnostics still reach the
	// daemon while .env has pending edits. Reload also pins its canonical source.
	args := []string{"--color", o.Color, "--socket", target.Socket}
	if o.JSON {
		args = append(args, "--json")
	}
	if command == "reload" || command == "restart-check" {
		args = append(args, "--env", target.Path)
	}
	args = append(args, strings.Fields(o.Command)...)
	if o.ExpectedSHA256 != "" {
		args = append(args, "--expected-sha256", o.ExpectedSHA256)
	}
	if command == "command" {
		args = append(args, "--timeout", commandTimeout(o.Timeout, target.Values).String(), "--")
		args = append(args, o.Args...)
	}
	if o.Raw {
		args = append(args, "--raw")
	}
	return args
}

type socketCall struct {
	method  string
	body    any
	timeout time.Duration
	limit   int64
}

func prepareSocketCall(o cliOptions, command string, target socketTarget) (socketCall, error) {
	call := socketCall{method: "GET", timeout: 20 * time.Second, limit: 65536}
	if command != "status" && command != "config" && command != "metrics" {
		call.method = "POST"
	}
	var err error
	switch command {
	case "config":
		call.limit = maxConfigResponseBytes
	case "metrics":
		call.limit = maxMetricsResponseBytes
	case "reload", "restart-check":
		call.body, err = prepareReloadRequest(target.Path, o.ExpectedSHA256)
	case "command":
		var text string
		text, err = operatorCommand(o.Args)
		if err != nil {
			return call, err
		}
		var duration time.Duration
		duration, err = validatedCommandTimeout(o.Timeout, target.Values)
		call.body = commandRequest{Command: text, Timeout: duration.String()}
		call.timeout = duration + 3*time.Second
		call.limit = maxMetricsResponseBytes
	}
	return call, err
}

func prepareReloadRequest(path, expected string) (reloadRequest, error) {
	_, digest, err := readConfigSnapshot(path)
	if err != nil {
		return reloadRequest{}, err
	}
	if expected != "" {
		decoded, err := hex.DecodeString(expected)
		if err != nil || len(decoded) != 32 {
			return reloadRequest{}, errors.New("expected-sha256 must contain 64 hexadecimal characters")
		}
		if expected != digest {
			return reloadRequest{}, errors.New(".env changed after validation; prepare the request again")
		}
	}
	return reloadRequest{ExpectedSHA256: digest, EnvPath: path}, nil
}

func (call socketCall) send(ctx context.Context, socket, command string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, call.timeout)
	defer cancel()
	raw, err := socketRequest(requestCtx, socket, call.method, command, call.body, call.limit)
	if err != nil && command == "command" {
		return nil, fmt.Errorf("command request failed; it may already have executed and was not retried: %w", err)
	}
	return raw, err
}

func renderSocketReply(o cliOptions, command string, raw []byte, out *cliOutput) error {
	if command == "command" {
		var response commandResponse
		if json.Unmarshal(raw, &response) != nil {
			return errors.New("invalid command response; command was not retried")
		}
		return out.CommandReply(response.Reply)
	}
	if command == "metrics" {
		if o.Command == "stats" {
			return out.Stats(raw)
		}
		return out.Metrics(raw, o.Raw)
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return errors.New("gateway returned an invalid structured response")
	}
	return out.Result(titleWords(o.Command), data)
}

func validatedCommandTimeout(explicit time.Duration, values map[string]string) (time.Duration, error) {
	duration := commandTimeout(explicit, values)
	if duration <= 0 || duration > maxCommandTimeout {
		return 0, errors.New("command timeout must be positive and at most 5m")
	}
	return duration, nil
}

func commandTimeout(explicit time.Duration, values map[string]string) time.Duration {
	if explicit != 0 {
		return explicit
	}
	if text := values["MC_COMMAND_TIMEOUT"]; text != "" {
		value, err := time.ParseDuration(text)
		if err != nil {
			return -1
		}
		return value
	}
	return 30 * time.Second
}
