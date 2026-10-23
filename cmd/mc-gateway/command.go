package main

import (
	"context"
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

	"mc-gateway/internal/backend"
	"mc-gateway/internal/rcon"
)

// runCommand submits one operator command directly to the configured local
// RCON endpoint. It does not wake the server, replay commands, or invoke a shell.
func runCommand(args []string, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	flags := flag.NewFlagSet("command", flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", "/etc/mc-gateway/config.json", "configuration file")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time for one command; failed commands are not retried")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command, err := operatorCommand(flags.Args())
	if err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("command timeout must be positive")
	}
	// Validate the command before reading configuration or credentials, or
	// attempting a connection. Shell quoting is already resolved by the caller.
	cfg, err := backend.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load command configuration: %w", err)
	}
	password, err := backend.ReadPassword(cfg.RCONPasswordFile)
	if err != nil {
		return err
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	client := rcon.New(cfg.RCONAddress, password, *timeout)
	reply, err := client.Command(ctx, command)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("command interrupted or timed out; it may already have executed and was not retried: %w", ctx.Err())
		}
		if errors.Is(err, rcon.ErrAuthentication) {
			return errors.New("RCON authentication rejected; check the configured password file")
		}
		var operation *net.OpError
		if errors.As(err, &operation) && operation.Op == "dial" {
			return fmt.Errorf("Minecraft RCON is unavailable; the server may be asleep or starting. Use task start and wait for ready: %w", err)
		}
		return fmt.Errorf("RCON command failed; it may already have executed and was not retried: %w", err)
	}
	// Normal server output is preserved. Never expose the authentication secret,
	// even if a server plugin unexpectedly includes it in a command response.
	reply = strings.ReplaceAll(reply, password, "[REDACTED]")
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
	if strings.ContainsAny(command, "\x00\r\n") {
		return "", errors.New("provide one command without NULs or line breaks")
	}
	command = strings.TrimSpace(command)
	command = strings.TrimPrefix(command, "/")
	if strings.TrimSpace(command) == "" {
		return "", errors.New("a Minecraft command is required; for example: task command -- op PlayerName")
	}
	return command, nil
}
