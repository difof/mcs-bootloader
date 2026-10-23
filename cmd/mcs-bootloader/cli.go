package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/operator"
)

type cliOptions struct {
	Command                                                                    string
	Args                                                                       []string
	Env, Socket, Color, ExpectedSHA256, PID, Report                            string
	JSON, Help, Raw, Direct, Server, Follow, DryRun, Yes, TimeoutSet, LinesSet bool
	Timeout                                                                    time.Duration
	Lines                                                                      int
}

var serviceAccountPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

var commandGroups = map[string]bool{"config": true, "gateway": true}
var knownCommands = map[string]bool{
	"help": true, "version": true, "init": true, "install": true, "deploy": true, "remove": true,
	"status": true, "stats": true, "start": true, "wake": true, "stop": true, "sleep": true, "command": true, "metrics": true, "logs": true,
	"config": true, "config new": true, "config link": true, "config init": true, "config edit": true, "config show": true, "config live": true, "config check": true, "config reload": true,
	"gateway": true, "gateway start": true, "gateway stop": true, "gateway restart": true, "gateway configure": true,
	"serve": true, "check": true, "reload": true, "restart-check": true, "backend-start": true, "backend-stop": true,
}

func parseCLI(args []string, out *cliOutput) (cliOptions, error) {
	out.JSON = requestedJSON(args)
	o := cliOptions{Color: "auto", Lines: 0}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			o.Args = append(o.Args, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if o.Command == "" {
				o.Command = arg
				continue
			}
			if commandGroups[o.Command] {
				o.Command += " " + arg
				continue
			}
			o.Args = append(o.Args, arg)
			// From the first Minecraft word onward every byte is command data. Flags
			// must precede it; a player's text such as --json is never consumed.
			if o.Command == "command" {
				o.Args = append(o.Args, args[i+1:]...)
				break
			}
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		take := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], nil
		}
		boolean := func() (bool, error) {
			if !hasValue {
				return true, nil
			}
			v, e := strconv.ParseBool(value)
			if e != nil {
				return false, fmt.Errorf("%s expects true or false", name)
			}
			return v, nil
		}
		var err error
		switch name {
		case "--env", "--config":
			o.Env, err = take()
		case "--socket":
			o.Socket, err = take()
		case "--json":
			o.JSON, err = boolean()
			out.JSON = o.JSON
		case "--color":
			o.Color, err = take()
			out.Color = o.Color
		case "--help", "-h":
			o.Help, err = boolean()
		case "--version":
			o.Command = "version"
		case "--raw":
			o.Raw, err = boolean()
		case "--direct":
			o.Direct, err = boolean()
		case "--server":
			o.Server, err = boolean()
		case "--follow", "-f":
			o.Follow, err = boolean()
		case "--dry-run":
			o.DryRun, err = boolean()
		case "--yes":
			o.Yes, err = boolean()
		case "--lines", "-n":
			o.LinesSet = true
			var s string
			s, err = take()
			if err == nil {
				o.Lines, err = strconv.Atoi(s)
				if err != nil {
					err = errors.New("--lines must be a decimal integer")
				}
			}
		case "--timeout":
			o.TimeoutSet = true
			var s string
			s, err = take()
			if err == nil {
				o.Timeout, err = time.ParseDuration(s)
				if err != nil {
					err = errors.New("--timeout requires a duration such as 30s")
				}
			}
		case "--expected-sha256":
			o.ExpectedSHA256, err = take()
		case "--pid":
			o.PID, err = take()
		case "--report":
			o.Report, err = take()
		default:
			return o, fmt.Errorf("unknown flag %q; use --help", name)
		}
		if err != nil {
			return o, err
		}
	}
	if o.Command == "" {
		o.Command = "help"
	}
	if !knownCommands[o.Command] {
		return o, fmt.Errorf("unknown command %q; use --help", o.Command)
	}
	if o.Color != "auto" && o.Color != "always" && o.Color != "never" {
		return o, errors.New("--color must be auto, always, or never")
	}
	if o.Raw && (o.Command != "metrics" || o.JSON) {
		return o, errors.New("--raw is only available for metrics and cannot be combined with --json")
	}
	if (o.Server || o.Follow || o.LinesSet) && o.Command != "logs" {
		return o, errors.New("--server, --follow and --lines are only available for logs")
	}
	if o.LinesSet && (o.Lines < 1 || o.Lines > 100000) {
		return o, errors.New("--lines must be between 1 and 100000")
	}
	if o.Direct && o.Command != "command" {
		return o, errors.New("--direct is only available for command")
	}
	if o.TimeoutSet && (o.Timeout <= 0 || o.Timeout > maxCommandTimeout) {
		return o, errors.New("--timeout must be positive and at most 5m")
	}
	if o.TimeoutSet && o.Command != "command" {
		return o, errors.New("--timeout is only available for command")
	}
	if (o.DryRun || o.Yes) && o.Command != "remove" {
		return o, errors.New("--dry-run and --yes are only available for remove")
	}
	if o.DryRun && o.Yes {
		return o, errors.New("use --dry-run or --yes, not both")
	}
	if o.PID != "" && o.Command != "backend-stop" {
		return o, errors.New("--pid is only available for backend-stop")
	}
	if o.ExpectedSHA256 != "" && o.Command != "reload" && o.Command != "config reload" && o.Command != "restart-check" {
		return o, errors.New("--expected-sha256 is only available for reload or restart-check")
	}
	if o.Report != "" && o.Command != "install" {
		return o, errors.New("--report is only available for install")
	}
	return o, nil
}

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
	if o.Help || o.Command == "help" || o.Command == "config" || o.Command == "gateway" {
		return printHelp(o, out)
	}
	if o.Command == "version" {
		if len(o.Args) > 0 {
			return errors.New("version takes no arguments")
		}
		return out.Result("mcs-bootloader", map[string]any{"version": version})
	}
	if o.Command == "backend-start" || o.Command == "backend-stop" {
		return runBackend(o)
	}
	if o.Command == "serve" {
		if len(o.Args) > 0 {
			return errors.New("serve takes no arguments")
		}
		path, err := resolveEnv(o.Env, false)
		if err != nil {
			return err
		}
		return serveWithOutput(path, out)
	}
	if o.Command == "init" || o.Command == "config new" || o.Command == "config init" || o.Command == "config link" {
		return runOperator(ctx, o, out, args)
	}
	if o.Command == "command" {
		if _, err := operatorCommand(o.Args); err != nil {
			return err
		}
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
	if o.Command != "command" && len(o.Args) > 0 {
		return fmt.Errorf("%s takes no positional arguments", o.Command)
	}
	switch o.Command {
	case "install", "deploy", "remove", "config edit", "config show", "gateway start", "gateway stop", "gateway restart", "gateway configure":
		return runOperator(ctx, o, out, args)
	case "check", "config check":
		path, err := resolveEnv(o.Env, false)
		if err != nil {
			return err
		}
		cfg, err := backend.LoadConfig(path)
		if err != nil {
			return err
		}
		if _, err = cfg.Password(); err != nil {
			return err
		}
		result := map[string]any{"valid": true, "path": path, "message": "Configuration and password permissions are valid."}
		if o.Command == "config check" {
			raw, err := readBoundedFile(path, 65536)
			if err != nil {
				return err
			}
			values, err := backend.EnvValues(raw)
			if err != nil {
				return err
			}
			account := values["MC_SERVICE_USER"]
			if account == "" {
				account = "minecraft"
			}
			if !serviceAccountPattern.MatchString(account) || account == "root" {
				return errors.New("MC_SERVICE_USER must be an existing non-root account")
			}
			probe := exec.CommandContext(ctx, "sudo", "-n", "-u", account, "--", "/usr/bin/test", "-r", path)
			if err := probe.Run(); err != nil {
				return fmt.Errorf("configuration is not readable as service account %s; check its account, directory traversal, file permissions and sudo access", account)
			}
			result["service_account"] = account
			result["service_account_readable"] = true
		}
		return out.Result("Configuration", result)
	case "logs":
		return runLogs(ctx, o, out, args)
	default:
		return runSocketOperation(ctx, o, out, args)
	}
}

func resolveEnv(explicit string, create bool) (string, error) {
	path := explicit
	if path == "" {
		path = ".env"
		if !create {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				path = defaultConfigPath
			} else if err != nil {
				return "", err
			}
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if create {
		return absolute, nil
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("open .env: %w; use mcs-bootloader init to create one, or --env PATH", err)
	}
	return resolved, nil
}

func selfExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func runOperator(ctx context.Context, o cliOptions, out *cliOutput, original []string) error {
	action := strings.ReplaceAll(o.Command, " ", "-")
	if action == "init" {
		action = "config-new"
	}
	if action == "config-init" {
		action = "config-link"
	}
	create := action == "config-new" || action == "config-link"
	if create && len(o.Args) > 0 {
		if len(o.Args) > 1 || o.Env != "" {
			return errors.New("provide one destination path, or --env PATH")
		}
		o.Env = o.Args[0]
	}
	path, err := resolveEnv(o.Env, create)
	if err != nil {
		return err
	}
	root := action == "install" || action == "deploy" || action == "remove" || strings.HasPrefix(action, "gateway-")
	if root && os.Geteuid() != 0 {
		return reexecAs(ctx, "root", withEnvArgument(original, path), out)
	}
	executable, err := selfExecutable()
	if err != nil {
		return err
	}
	var helperArgs []string
	if o.Report != "" {
		report, err := filepath.Abs(o.Report)
		if err != nil {
			return err
		}
		helperArgs = []string{"--report", report}
	}
	if o.DryRun {
		helperArgs = append(helperArgs, "--dry-run")
	}
	if o.Yes {
		helperArgs = append(helperArgs, "--yes")
	}
	result, err := operator.Run(ctx, action, helperArgs, operator.Options{Env: path, Executable: executable, Stdin: os.Stdin, Stdout: out.Out, Stderr: out.Err})
	if err != nil {
		return err
	}
	var data any
	if err = json.Unmarshal(result, &data); err != nil {
		return errors.New("operator returned an invalid structured result")
	}
	return out.Result(titleWords(o.Command), data)
}

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

func runSocketOperation(ctx context.Context, o cliOptions, out *cliOutput, original []string) error {
	command := map[string]string{"status": "status", "stats": "metrics", "start": "wake", "wake": "wake", "stop": "sleep", "sleep": "sleep", "config live": "config", "reload": "reload", "config reload": "reload", "restart-check": "restart-check", "metrics": "metrics", "command": "command"}[o.Command]
	if command == "" {
		return fmt.Errorf("unsupported operation %s", o.Command)
	}
	var cfg backend.Config
	var values map[string]string
	path := ""
	if o.Env != "" || o.Socket == "" || command == "reload" || command == "restart-check" || o.Direct {
		var err error
		path, err = resolveEnv(o.Env, false)
		if err != nil {
			return err
		}
		if command == "reload" || command == "restart-check" || o.Direct {
			cfg, err = backend.LoadConfig(path)
			if err != nil {
				return err
			}
			if o.Socket == "" {
				o.Socket = cfg.AdminSocket
			}
		} else {
			values, err = readOperatorValues(path)
			if err != nil {
				if errors.Is(err, os.ErrPermission) && os.Geteuid() != 0 {
					return reexecAs(ctx, "root", withEnvArgument(original, path), out)
				}
				return err
			}
			if o.Socket == "" {
				o.Socket = values["MC_ADMIN_SOCKET"]
				if o.Socket == "" {
					o.Socket = backend.DefaultConfig().AdminSocket
				}
			}
		}
	}
	if o.Socket != "" && (!filepath.IsAbs(o.Socket) || filepath.Clean(o.Socket) == "/" || strings.ContainsAny(o.Socket, "\x00\r\n")) {
		return errors.New("admin socket must be an absolute non-root path without control characters")
	}
	if command == "command" && o.Direct {
		if raw, err := readBoundedFile(path, 65536); err == nil && !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
			values, err = backend.OperatorValues(raw)
			if err != nil {
				return err
			}
		}
		var reply strings.Builder
		duration := commandTimeout(o.Timeout, values)
		if duration <= 0 || duration > maxCommandTimeout {
			return errors.New("command timeout must be positive and at most 5m")
		}
		if err := runCommand(append([]string{"--config", path, "--timeout", duration.String(), "--"}, o.Args...), &reply); err != nil {
			return err
		}
		return out.CommandReply(strings.TrimSuffix(reply.String(), "\n"))
	}
	user, err := socketIdentity(o.Socket)
	if err != nil {
		return err
	}
	if user != "" {
		// Pin the already selected socket before root re-exec; diagnostics use
		// the running daemon even when the on-disk configuration has pending edits.
		// Reload alone needs the validated canonical source path as well.
		forwarded := []string{"--color", o.Color, "--socket", o.Socket}
		if o.JSON {
			forwarded = append(forwarded, "--json")
		}
		if command == "reload" || command == "restart-check" {
			forwarded = append(forwarded, "--env", path)
		}
		forwarded = append(forwarded, strings.Fields(o.Command)...)
		if o.ExpectedSHA256 != "" {
			forwarded = append(forwarded, "--expected-sha256", o.ExpectedSHA256)
		}
		if command == "command" {
			forwarded = append(forwarded, "--timeout", commandTimeout(o.Timeout, values).String(), "--")
			forwarded = append(forwarded, o.Args...)
		}
		if o.Raw {
			forwarded = append(forwarded, "--raw")
		}
		return reexecAs(ctx, user, forwarded, out)
	}
	method := "GET"
	if command != "status" && command != "config" && command != "metrics" {
		method = "POST"
	}
	var body any
	timeout := 20 * time.Second
	limit := int64(65536)
	if command == "config" {
		limit = maxConfigResponseBytes
	}
	if command == "metrics" {
		limit = maxMetricsResponseBytes
	}
	if command == "reload" || command == "restart-check" {
		_, digest, err := readConfigSnapshot(path)
		if err != nil {
			return err
		}
		if o.ExpectedSHA256 != "" {
			decoded, err := hex.DecodeString(o.ExpectedSHA256)
			if err != nil || len(decoded) != 32 {
				return errors.New("expected-sha256 must contain 64 hexadecimal characters")
			}
			if o.ExpectedSHA256 != digest {
				return errors.New(".env changed after validation; prepare the request again")
			}
		}
		body = reloadRequest{ExpectedSHA256: digest, EnvPath: path}
	}
	if command == "command" {
		text, err := operatorCommand(o.Args)
		if err != nil {
			return err
		}
		duration := commandTimeout(o.Timeout, values)
		if duration <= 0 || duration > maxCommandTimeout {
			return errors.New("command timeout must be positive and at most 5m")
		}
		body = commandRequest{Command: text, Timeout: duration.String()}
		timeout = duration + 3*time.Second
		limit = maxMetricsResponseBytes
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := socketRequest(requestCtx, o.Socket, method, command, body, limit)
	if err != nil {
		if command == "command" {
			return fmt.Errorf("command request failed; it may already have executed and was not retried: %w", err)
		}
		return err
	}
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

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func runBackend(o cliOptions) error {
	service, err := currentBackendServiceContext()
	if err != nil {
		return err
	}
	return runBackendInService(o, service)
}

func runBackendInService(o cliOptions, service backendServiceContext) error {
	if err := service.validateIdentity(); err != nil {
		return err
	}
	if len(o.Args) > 0 {
		return errors.New("unexpected backend helper arguments")
	}
	path := o.Env
	if path == "" {
		path = defaultConfigPath
	}
	socket := o.Socket
	if socket == "" {
		socket = "/run/mcs-bootloader/control.sock"
	}
	pid := 0
	if o.PID != "" {
		var err error
		pid, err = strconv.Atoi(o.PID)
		if err != nil || pid < 0 {
			return errors.New("invalid backend PID")
		}
	}
	if o.Command == "backend-stop" && pid == 0 {
		return nil
	}
	cfg, err := appliedBackendConfig(socket)
	if err != nil {
		disk, diskErr := backend.LoadConfig(path)
		if diskErr != nil {
			return errors.New("applied configuration is unavailable and .env could not be validated")
		}
		if disk.AdminSocket != socket {
			cfg, err = appliedBackendConfig(disk.AdminSocket)
		}
		if err != nil {
			if o.Command == "backend-start" {
				return errors.New("gateway applied settings are unavailable; start the gateway before Minecraft")
			}
			cfg = disk
		}
	}
	if err := service.validateService(cfg.Service); err != nil {
		return err
	}
	if o.Command == "backend-start" {
		return backend.StartServer(cfg)
	}
	return gracefulBackendStop(cfg, pid)
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

func readOperatorValues(path string) (map[string]string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0640) {
		return nil, errors.New(".env must be a regular file with private 0600 or 0640 permissions")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	return backend.OperatorValues(raw)
}
