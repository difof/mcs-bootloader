package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func reexecAs(ctx context.Context, user string, args []string, out *cliOutput) error {
	self, err := selfExecutable()
	if err != nil {
		return err
	}
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor && out.Color == "auto" {
		args = replaceGlobalValue(args, []string{"--color"}, "--color", "never")
	}
	command := exec.CommandContext(ctx, "sudo", append([]string{"-n", "-u", user, "--", self}, args...)...)
	command.Stdin = os.Stdin
	command.Stdout = out.Out
	command.Stderr = out.Err
	// sudo forwards TERM to the privileged child. Killing sudo immediately
	// could strand an installation transaction before its rollback handler runs.
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	// Wait for the child to finish its cleanup; there is no forced-kill deadline
	// on a privileged installation transaction.
	var diagnostic boundedText
	if out.JSON {
		command.Stderr = &diagnostic
	}
	err = command.Run()
	if out.JSON && diagnostic.Len() > 0 {
		raw := strings.TrimSpace(diagnostic.String())
		if json.Valid([]byte(raw)) {
			_, _ = fmt.Fprintln(out.Err, raw)
		} else if err != nil {
			return fmt.Errorf("privilege escalation failed: %s", cleanText(raw))
		} else {
			_ = encodeJSON(out.Err, map[string]any{"warning": cleanText(raw)})
		}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if code < 1 {
			code = 1
		}
		return &childExitError{Code: code}
	}
	if err != nil {
		return fmt.Errorf("run as %s: %w", user, err)
	}
	return nil
}

// Pin an absolute source before sudo, without forwarding a second relative
// --env that would take precedence. Only management/log commands use this.
func withEnvArgument(args []string, path string) []string {
	return replaceGlobalValue(args, []string{"--env", "--config"}, "--env", path)
}

func replaceGlobalValue(args []string, aliases []string, name, value string) []string {
	result := []string{name, value}
	command := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...)
		}
		if !strings.HasPrefix(arg, "-") {
			if command == "" {
				command = arg
			} else if commandGroups[command] {
				command += " " + arg
			} else if command == "command" {
				return append(result, args[i:]...)
			}
			result = append(result, arg)
			continue
		}
		flag, _, equals := strings.Cut(arg, "=")
		replace := false
		for _, alias := range aliases {
			if flag == alias {
				replace = true
				break
			}
		}
		if replace {
			if !equals {
				i++
			}
			continue
		}
		result = append(result, arg)
		if optionValueFlags[flag] && !equals && i+1 < len(args) {
			i++
			result = append(result, args[i])
		}
	}
	return result
}
