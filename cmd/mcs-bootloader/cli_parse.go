package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cliOptions struct {
	Command                                                                    string
	Args                                                                       []string
	Env, Socket, Color, ExpectedSHA256, PID, Report                            string
	JSON, Help, Raw, Direct, Server, Follow, DryRun, Yes, TimeoutSet, LinesSet bool
	Timeout                                                                    time.Duration
	Lines                                                                      int
}

var commandGroups = map[string]bool{"config": true, "gateway": true}
var knownCommands = map[string]bool{
	"help": true, "version": true, "license": true, "init": true, "install": true, "deploy": true, "remove": true,
	"status": true, "stats": true, "start": true, "wake": true, "stop": true, "sleep": true, "command": true, "metrics": true, "logs": true,
	"config": true, "config new": true, "config link": true, "config init": true, "config edit": true, "config show": true, "config live": true, "config check": true, "config reload": true,
	"gateway": true, "gateway start": true, "gateway stop": true, "gateway restart": true, "gateway configure": true,
	"serve": true, "check": true, "reload": true, "restart-check": true, "backend-start": true, "backend-stop": true,
}

func parseCLI(args []string, out *cliOutput) (cliOptions, error) {
	out.JSON = requestedJSON(args)
	o := cliOptions{Color: "auto"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			o.Args = append(o.Args, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if o.Command == "" {
				o.Command = arg
			} else if commandGroups[o.Command] {
				o.Command += " " + arg
			} else {
				o.Args = append(o.Args, arg)
				// From the first Minecraft word onward every byte is command data.
				if o.Command == "command" {
					o.Args = append(o.Args, args[i+1:]...)
					break
				}
			}
			continue
		}
		if err := parseCLIFlag(args, &i, &o, out); err != nil {
			return o, err
		}
	}
	if o.Command == "" {
		o.Command = "help"
	}
	return o, o.validate()
}

type cliFlag struct {
	name, value string
	hasValue    bool
}

func (f cliFlag) take(args []string, index *int) (string, error) {
	if f.hasValue {
		return f.value, nil
	}
	if *index+1 >= len(args) {
		return "", fmt.Errorf("%s requires a value", f.name)
	}
	*index++
	return args[*index], nil
}

func (f cliFlag) boolean() (bool, error) {
	if !f.hasValue {
		return true, nil
	}
	value, err := strconv.ParseBool(f.value)
	if err != nil {
		return false, fmt.Errorf("%s expects true or false", f.name)
	}
	return value, nil
}

func parseCLIFlag(args []string, index *int, o *cliOptions, out *cliOutput) error {
	name, value, hasValue := strings.Cut(args[*index], "=")
	flag := cliFlag{name: name, value: value, hasValue: hasValue}
	if target := o.stringFlag(name); target != nil {
		var err error
		*target, err = flag.take(args, index)
		if name == "--color" {
			out.Color = o.Color
		}
		return err
	}
	if target := o.booleanFlag(name); target != nil {
		var err error
		*target, err = flag.boolean()
		if name == "--json" {
			out.JSON = o.JSON
		}
		return err
	}
	switch name {
	case "--version":
		o.Command = "version"
		return nil
	case "--lines", "-n":
		o.LinesSet = true
		value, err := flag.take(args, index)
		if err != nil {
			return err
		}
		o.Lines, err = strconv.Atoi(value)
		if err != nil {
			return errors.New("--lines must be a decimal integer")
		}
	case "--timeout":
		o.TimeoutSet = true
		value, err := flag.take(args, index)
		if err != nil {
			return err
		}
		o.Timeout, err = time.ParseDuration(value)
		if err != nil {
			return errors.New("--timeout requires a duration such as 30s")
		}
	default:
		return fmt.Errorf("unknown flag %q; use --help", name)
	}
	return nil
}

func (o *cliOptions) stringFlag(name string) *string {
	switch name {
	case "--env", "--config":
		return &o.Env
	case "--socket":
		return &o.Socket
	case "--color":
		return &o.Color
	case "--expected-sha256":
		return &o.ExpectedSHA256
	case "--pid":
		return &o.PID
	case "--report":
		return &o.Report
	}
	return nil
}

func (o *cliOptions) booleanFlag(name string) *bool {
	switch name {
	case "--json":
		return &o.JSON
	case "--help", "-h":
		return &o.Help
	case "--raw":
		return &o.Raw
	case "--direct":
		return &o.Direct
	case "--server":
		return &o.Server
	case "--follow", "-f":
		return &o.Follow
	case "--dry-run":
		return &o.DryRun
	case "--yes":
		return &o.Yes
	}
	return nil
}

func (o cliOptions) validate() error {
	if !knownCommands[o.Command] {
		return fmt.Errorf("unknown command %q; use --help", o.Command)
	}
	rules := []struct {
		invalid bool
		message string
	}{
		{o.Color != "auto" && o.Color != "always" && o.Color != "never", "--color must be auto, always, or never"},
		{o.Raw && (o.Command != "metrics" || o.JSON), "--raw is only available for metrics and cannot be combined with --json"},
		{(o.Server || o.Follow || o.LinesSet) && o.Command != "logs", "--server, --follow and --lines are only available for logs"},
		{o.LinesSet && (o.Lines < 1 || o.Lines > 100000), "--lines must be between 1 and 100000"},
		{o.Direct && o.Command != "command", "--direct is only available for command"},
		{o.TimeoutSet && (o.Timeout <= 0 || o.Timeout > maxCommandTimeout), "--timeout must be positive and at most 5m"},
		{o.TimeoutSet && o.Command != "command", "--timeout is only available for command"},
		{(o.DryRun || o.Yes) && o.Command != "remove", "--dry-run and --yes are only available for remove"},
		{o.DryRun && o.Yes, "use --dry-run or --yes, not both"},
		{o.PID != "" && o.Command != "backend-stop", "--pid is only available for backend-stop"},
		{o.ExpectedSHA256 != "" && o.Command != "reload" && o.Command != "config reload" && o.Command != "restart-check", "--expected-sha256 is only available for reload or restart-check"},
		{o.Report != "" && o.Command != "install", "--report is only available for install"},
	}
	for _, rule := range rules {
		if rule.invalid {
			return errors.New(rule.message)
		}
	}
	return nil
}

var optionValueFlags = map[string]bool{"--env": true, "--config": true, "--socket": true, "--color": true, "--lines": true, "-n": true, "--timeout": true, "--expected-sha256": true, "--pid": true, "--report": true}

// Error formatting is chosen before validation, including `--bad --json`.
// Respect command operands and option values so literal Minecraft text cannot
// accidentally enable machine output.
func requestedJSON(args []string) bool {
	enabled := false
	command := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			if command == "" {
				command = arg
				continue
			}
			if commandGroups[command] {
				command += " " + arg
				continue
			}
			if command == "command" {
				break
			}
			continue
		}
		name, value, equals := strings.Cut(arg, "=")
		if name == "--json" {
			if !equals {
				enabled = true
			} else if b, err := strconv.ParseBool(value); err == nil {
				enabled = b
			}
		}
		if optionValueFlags[name] && !equals {
			i++
		}
	}
	return enabled
}
