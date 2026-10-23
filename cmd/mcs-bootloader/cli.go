package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func executeCLI(ctx context.Context, args []string, out *cliOutput) error {
	if len(args) > 0 && args[0] == "_task" {
		recovered, err := taskArguments("/proc", os.Getppid())
		if err != nil {
			return err
		}
		args = append(append([]string(nil), args[1:]...), recovered...)
	}
	o, err := parseCLI(args, out)
	if err != nil {
		return err
	}
	return dispatchCLI(ctx, o, args, out)
}

func dispatchCLI(ctx context.Context, o cliOptions, original []string, out *cliOutput) error {
	if o.Help || o.Command == "help" || o.Command == "config" || o.Command == "gateway" {
		return printHelp(o, out)
	}
	switch o.Command {
	case "version":
		if len(o.Args) > 0 {
			return errors.New("version takes no arguments")
		}
		return out.Result("mcs-bootloader", map[string]any{"version": version})
	case "backend-start", "backend-stop":
		return runBackend(o)
	case "license":
		return printLicense(o, out)
	case "serve":
		return runForeground(o, out)
	case "init", "config new", "config init", "config link":
		return runOperator(ctx, o, out, original)
	}
	if err := o.prepareOperands(); err != nil {
		return err
	}
	switch o.Command {
	case "install", "deploy", "remove", "config edit", "config show", "gateway start", "gateway stop", "gateway restart", "gateway configure":
		return runOperator(ctx, o, out, original)
	case "check", "config check":
		return runConfigCheck(ctx, o, out)
	case "logs":
		return runLogs(ctx, o, out, original)
	default:
		return runSocketOperation(ctx, o, out, original)
	}
}

func runForeground(o cliOptions, out *cliOutput) error {
	if len(o.Args) > 0 {
		return errors.New("serve takes no arguments")
	}
	path, err := resolveEnv(o.Env, false)
	if err != nil {
		return err
	}
	return serveWithOutput(path, out)
}

func (o *cliOptions) prepareOperands() error {
	if o.Command == "command" {
		_, err := operatorCommand(o.Args)
		return err
	}
	if o.Command == "config reload" || o.Command == "reload" {
		if len(o.Args) > 1 {
			return errors.New("config reload accepts at most one .env path")
		}
		if len(o.Args) == 1 {
			if o.Env != "" {
				return errors.New("use either --env or the reload path, not both")
			}
			o.Env = o.Args[0]
			o.Args = nil
		}
	}
	if len(o.Args) > 0 {
		return fmt.Errorf("%s takes no positional arguments", o.Command)
	}
	return nil
}
