package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEnvBytes = 64 * 1024

var envAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$`)
var envInlineComment = regexp.MustCompile(`[ \t]+#`)

// LoadEnv parses the shared .env as data. It does not execute a shell, expand
// variables, or inherit process environment values. LoadConfig additionally
// checks the file's permissions; this entry point is for private snapshots.
func LoadEnv(data []byte) (Config, error) {
	c := DefaultConfig()
	c.RCONPasswordFile = ""
	if len(data) > maxEnvBytes {
		return c, fmt.Errorf("environment file exceeds 64 KiB")
	}
	if i := bytes.IndexAny(data, "\x00\r"); i >= 0 {
		return c, envError(bytes.Count(data[:i], []byte("\n"))+1, "", "NUL and carriage return are not allowed; use LF line endings")
	}
	if !utf8.Valid(data) {
		return c, fmt.Errorf("environment file must contain valid UTF-8")
	}
	stringFields := map[string]*string{
		"MC_LISTEN":            &c.Listen,
		"MC_BACKEND_ADDRESS":   &c.BackendAddress,
		"MC_RCON_ADDRESS":      &c.RCONAddress,
		"MC_RCON_PASSWORD":     &c.RCONPassword,
		"MC_BACKEND_SERVICE":   &c.Service,
		"MC_STATE_DIRECTORY":   &c.StateDirectory,
		"MC_ADMIN_SOCKET":      &c.AdminSocket,
		"MC_SERVER_DIRECTORY":  &c.ServerDirectory,
		"MC_LAUNCH_EXECUTABLE": &c.LaunchExecutable,
		"MC_VERSION":           &c.Version,
		"MC_BOOT_MESSAGE":      &c.BootMessage,
		"MC_SLEEPING_MOTD":     &c.SleepingMOTD,
		"MC_STARTING_MOTD":     &c.StartingMOTD,
		"MC_STOPPING_MOTD":     &c.StoppingMOTD,
		"MC_UNKNOWN_MOTD":      &c.UnknownMOTD,
		"MC_FAILED_MOTD":       &c.FailedMOTD,
		"MC_MOTD":              &c.MOTD,
		"MC_RESTART_MESSAGE":   &c.RestartMessage,
		"MC_WHITELIST_MESSAGE": &c.WhitelistMessage,
		"MC_METRICS_LISTEN":    &c.MetricsListen,
		"MC_LOG_LEVEL":         &c.LogLevel,
	}
	durationFields := map[string]*Duration{
		"MC_IDLE_TIMEOUT":               &c.IdleTimeout,
		"MC_POLL_INTERVAL":              &c.PollInterval,
		"MC_OPERATION_TIMEOUT":          &c.OperationTimeout,
		"MC_STARTUP_TIMEOUT":            &c.StartupTimeout,
		"MC_SHUTDOWN_WARN_AFTER":        &c.ShutdownWarnAfter,
		"MC_WAKE_COOLDOWN":              &c.WakeCooldown,
		"MC_HANDSHAKE_TIMEOUT":          &c.HandshakeTimeout,
		"MC_DIAL_TIMEOUT":               &c.DialTimeout,
		"MC_RESTART_GRACE":              &c.RestartGrace,
		"MC_METRICS_POLL_INTERVAL":      &c.MetricsPollInterval,
		"MC_METRICS_PLAYER_INTERVAL":    &c.MetricsPlayerInterval,
		"MC_METRICS_DIRECTORY_INTERVAL": &c.MetricsDirectoryInterval,
		"MC_METRICS_DIRECTORY_TIMEOUT":  &c.MetricsDirectoryTimeout,
	}
	booleanFields := map[string]*bool{"MC_OVERRIDE_MOTD": &c.OverrideMOTD, "MC_METRICS_ENABLED": &c.MetricsEnabled, "MC_METRICS_PLAYER_DETAILS": &c.MetricsPlayerDetails}
	var logLines int
	integerFields := map[string]*int{
		"MC_PROTOCOL":        &c.Protocol,
		"MC_MAX_PLAYERS":     &c.MaxPlayers,
		"MC_MAX_CONNECTIONS": &c.MaxConnections,
		"MC_LOG_LINES":       &logLines,
	}
	taskFields := map[string]bool{
		"MC_BINARY": true, "MC_CONFIG": true, "MC_SERVICE_USER": true,
		"MC_GATEWAY_SERVICE": true, "MC_LOG_NAMESPACE": true,
		"MC_COMMAND_TIMEOUT": true,
	}
	seen := make(map[string]bool)
	for lineIndex, line := range strings.Split(string(data), "\n") {
		number := lineIndex + 1
		line = strings.Trim(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[6:], " \t")
		}
		parts := envAssignment.FindStringSubmatch(line)
		if parts == nil {
			return c, envError(number, "", "expected KEY=value")
		}
		key, literal := parts[1], parts[2]
		if stringFields[key] == nil && durationFields[key] == nil && integerFields[key] == nil && booleanFields[key] == nil && !taskFields[key] && key != "MC_LAUNCH_ARGS" && key != "MC_METRICS_DIRECTORIES" {
			return c, envError(number, key, "unknown setting")
		}
		if seen[key] {
			return c, envError(number, key, "duplicate setting")
		}
		seen[key] = true
		value, err := parseEnvValue(literal)
		if err != nil {
			return c, envError(number, key, err.Error())
		}
		if target := booleanFields[key]; target != nil {
			switch strings.ToLower(value) {
			case "true":
				*target = true
			case "false":
				*target = false
			default:
				return c, envError(number, key, "must be true or false")
			}
			continue
		}
		if target := integerFields[key]; target != nil {
			if !decimalDigits(value) {
				return c, envError(number, key, "must be a positive decimal integer")
			}
			// Normalize before parsing so an arbitrary number of leading zeroes
			// behaves identically to the management parser.
			digits := strings.TrimLeft(value, "0")
			if digits == "" {
				digits = "0"
			}
			n, err := strconv.ParseInt(digits, 10, strconv.IntSize)
			if err != nil || n < 1 {
				return c, envError(number, key, "integer must be positive and within range")
			}
			*target = int(n)
			continue
		}
		if err := validateEnvString(key, value); err != nil {
			return c, envError(number, key, err.Error())
		}
		if key == "MC_METRICS_DIRECTORIES" {
			directories, err := parseMetricsDirectories(value)
			if err != nil {
				return c, envError(number, key, err.Error())
			}
			c.MetricsDirectories = directories
			continue
		}
		if key == "MC_LAUNCH_ARGS" {
			var encoded []json.RawMessage
			if err := json.Unmarshal([]byte(value), &encoded); err != nil || encoded == nil || len(encoded) > maxLaunchArgs || !validJSONSurrogates(value) {
				return c, envError(number, key, "must be a JSON array containing at most 256 valid Unicode strings")
			}
			args := make([]string, len(encoded))
			for i, raw := range encoded {
				if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &args[i]) != nil {
					return c, envError(number, key, "every array item must be a string")
				}
			}
			c.LaunchArgs = args
			continue
		}
		if target := stringFields[key]; target != nil {
			*target = value
			continue
		}
		if target := durationFields[key]; target != nil {
			d, err := time.ParseDuration(value)
			if err != nil {
				return c, envError(number, key, "must use valid Go duration syntax")
			}
			*target = Duration(d)
		}
	}
	if !seen["MC_RCON_PASSWORD"] {
		return c, fmt.Errorf("MC_RCON_PASSWORD is required in .env")
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("environment configuration: %w", err)
	}
	return c, nil
}

func envError(line int, key, message string) error {
	if key == "" {
		return fmt.Errorf("line %d: %s", line, message)
	}
	return fmt.Errorf("line %d: %s: %s", line, key, message)
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validateEnvString(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("must not be empty")
	}
	message := key == "MC_BOOT_MESSAGE" || key == "MC_SLEEPING_MOTD" || key == "MC_STARTING_MOTD" || key == "MC_STOPPING_MOTD" || key == "MC_UNKNOWN_MOTD" || key == "MC_FAILED_MOTD" || key == "MC_MOTD" || key == "MC_WHITELIST_MESSAGE"
	if strings.ContainsAny(value, "\x00\r") {
		return fmt.Errorf("must not contain NUL or carriage return")
	}
	if !message && strings.Contains(value, "\n") {
		return fmt.Errorf("line breaks are allowed only in messages")
	}
	for _, r := range value {
		if r < 32 && r != '\n' && r != '\t' {
			return fmt.Errorf("contains an unsupported control character")
		}
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("must contain valid Unicode")
	}
	return nil
}

func parseEnvValue(literal string) (string, error) {
	if strings.HasPrefix(literal, "'") {
		end := strings.IndexByte(literal[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single-quoted value; multiline values are not supported")
		}
		end++
		if err := checkEnvTail(literal[end+1:]); err != nil {
			return "", err
		}
		return literal[1:end], nil
	}
	if strings.HasPrefix(literal, `"`) {
		var value string
		d := json.NewDecoder(strings.NewReader(literal))
		if err := d.Decode(&value); err != nil {
			return "", fmt.Errorf("invalid JSON-quoted string; multiline values are not supported")
		}
		end := int(d.InputOffset())
		if !validJSONSurrogates(literal[:end]) {
			return "", fmt.Errorf("must contain valid Unicode")
		}
		if err := checkEnvTail(literal[end:]); err != nil {
			return "", err
		}
		return value, nil
	}
	if strings.HasPrefix(literal, "#") {
		return "", nil
	}
	if cut := envInlineComment.FindStringIndex(literal); cut != nil {
		literal = literal[:cut[0]]
	}
	return strings.TrimRight(literal, " \t"), nil
}

func checkEnvTail(tail string) error {
	if strings.Trim(tail, " \t") == "" {
		return nil
	}
	if (tail[0] == ' ' || tail[0] == '\t') && strings.HasPrefix(strings.TrimLeft(tail, " \t"), "#") {
		return nil
	}
	return fmt.Errorf("unexpected text after quoted value")
}

// encoding/json replaces malformed UTF-16 with U+FFFD. Reject it instead so
// the Go and Python readers agree and no credential is silently changed.
func validJSONSurrogates(literal string) bool {
	for i := 0; i < len(literal); i++ {
		if literal[i] != '\\' {
			continue
		}
		i++
		if i >= len(literal) {
			return false
		}
		if literal[i] != 'u' {
			continue
		}
		if i+4 >= len(literal) {
			return false
		}
		n, err := strconv.ParseUint(literal[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xDC00 && n <= 0xDFFF {
			return false
		}
		if n >= 0xD800 && n <= 0xDBFF {
			if i+6 >= len(literal) || literal[i+1] != '\\' || literal[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(literal[i+3:i+7], 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

func parseMetricsDirectories(value string) (map[string]string, error) {
	fail := func() (map[string]string, error) {
		return nil, fmt.Errorf("must be a JSON object of unique names to directory path strings")
	}
	if !validJSONSurrogates(value) {
		return fail()
	}
	d := json.NewDecoder(strings.NewReader(value))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fail()
	}
	result := make(map[string]string)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return fail()
		}
		key, ok := token.(string)
		if !ok {
			return fail()
		}
		if _, exists := result[key]; exists {
			return fail()
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || len(raw) == 0 || raw[0] != '"' {
			return fail()
		}
		var path string
		if json.Unmarshal(raw, &path) != nil {
			return fail()
		}
		result[key] = path
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return fail()
	}
	if d.Decode(new(any)) != io.EOF {
		return fail()
	}
	if err := ValidateMetricsDirectories(result); err != nil {
		return nil, err
	}
	return result, nil
}
