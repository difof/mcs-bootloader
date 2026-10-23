package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

// runCommand submits one command through the private socket (or direct local
// RCON for recovery). It never wakes the server, retries, or invokes a shell.
func runCommand(args []string, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	flags := flag.NewFlagSet("command", flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", defaultConfigPath, "configuration file for direct recovery mode")
	socket := flags.String("socket", "", "gateway control socket; uses applied configuration")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time for one command; failed commands are not retried")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command, err := operatorCommand(flags.Args())
	if err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > maxCommandTimeout {
		return errors.New("command timeout must be positive and at most 5m")
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *socket != "" {
		ctx, cancel := context.WithTimeout(signalCtx, *timeout+3*time.Second)
		defer cancel()
		data, err := socketRequest(ctx, *socket, "POST", "command", commandRequest{Command: command, Timeout: timeout.String()}, 8*1024*1024)
		if err != nil {
			return fmt.Errorf("command request failed; it may already have executed and was not retried: %w", err)
		}
		var response commandResponse
		if json.Unmarshal(data, &response) != nil {
			return errors.New("invalid command response; command was not retried")
		}
		return writeCommandReply(output, response.Reply)
	}
	// Validate before reading credentials or attempting a connection.
	cfg, err := backend.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load command configuration: %w", err)
	}
	password, err := cfg.Password()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	client := rcon.New(cfg.RCONAddress, password, *timeout)
	reply, err := client.Command(ctx, command)
	if err != nil {
		return operatorError(ctx, err)
	}
	// Normal server output is preserved. Never expose the authentication secret,
	// even if a server plugin unexpectedly includes it in a command response.
	reply = strings.ReplaceAll(reply, password, "[REDACTED]")
	return writeCommandReply(output, reply)
}

func operatorError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("command interrupted or timed out; it may already have executed and was not retried: %w", ctx.Err())
	}
	if errors.Is(err, rcon.ErrAuthentication) {
		return errors.New("RCON authentication rejected; check MC_RCON_PASSWORD in .env")
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" {
		return fmt.Errorf("Minecraft RCON is unavailable; the server may be asleep or starting. Use mcs-bootloader start and wait for ready: %w", err)
	}
	return fmt.Errorf("RCON command failed; it may already have executed and was not retried: %w", err)
}

func writeCommandReply(output io.Writer, reply string) error {
	if _, err := io.WriteString(output, reply); err != nil {
		return fmt.Errorf("command completed but its reply could not be written; it was not retried: %w", err)
	}
	if !strings.HasSuffix(reply, "\n") {
		if _, err := io.WriteString(output, "\n"); err != nil {
			return fmt.Errorf("command completed but its reply could not be written; it was not retried: %w", err)
		}
	}
	return nil
}

func operatorCommand(args []string) (string, error) {
	command := strings.Join(args, " ")
	if len(command) > 32*1024 {
		return "", errors.New("command exceeds 32 KiB")
	}
	if strings.ContainsAny(command, "\x00\r\n") {
		return "", errors.New("provide one command without NULs or line breaks")
	}
	command = strings.TrimSpace(command)
	command = strings.TrimPrefix(command, "/")
	if strings.TrimSpace(command) == "" {
		return "", errors.New("a Minecraft command is required; for example: mcs-bootloader command -- op PlayerName")
	}
	return command, nil
}
