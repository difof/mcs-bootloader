package backend

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Field bindings belong to one configuration snapshot, so a failed parse cannot
// update shared defaults or the running daemon's configuration.
type envFields struct {
	strings   map[string]*string
	durations map[string]*Duration
	booleans  map[string]*bool
	integers  map[string]*int
	operator  map[string]bool
}

func newEnvFields(c *Config) envFields {
	strings := map[string]*string{
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
	durations := map[string]*Duration{
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
	booleans := map[string]*bool{"MC_OVERRIDE_MOTD": &c.OverrideMOTD, "MC_METRICS_ENABLED": &c.MetricsEnabled, "MC_METRICS_PLAYER_DETAILS": &c.MetricsPlayerDetails}
	var logLines int
	integers := map[string]*int{
		"MC_PROTOCOL":        &c.Protocol,
		"MC_MAX_PLAYERS":     &c.MaxPlayers,
		"MC_MAX_CONNECTIONS": &c.MaxConnections,
		"MC_LOG_LINES":       &logLines,
	}
	operator := map[string]bool{
		"MC_BINARY": true, "MC_CONFIG": true, "MC_SERVICE_USER": true,
		"MC_GATEWAY_SERVICE": true, "MC_LOG_NAMESPACE": true,
		"MC_COMMAND_TIMEOUT": true,
	}
	return envFields{strings: strings, durations: durations, booleans: booleans, integers: integers, operator: operator}
}

func (f envFields) known(key string) bool {
	return f.strings[key] != nil || f.durations[key] != nil || f.booleans[key] != nil ||
		f.integers[key] != nil || f.operator[key] || key == "MC_LAUNCH_ARGS" || key == "MC_METRICS_DIRECTORIES"
}

func (f envFields) assign(c *Config, key, value string) error {
	if target := f.booleans[key]; target != nil {
		switch strings.ToLower(value) {
		case "true":
			*target = true
		case "false":
			*target = false
		default:
			return fmt.Errorf("must be true or false")
		}
		return nil
	}
	if target := f.integers[key]; target != nil {
		n, err := parseEnvInteger(value)
		if err == nil {
			*target = n
		}
		return err
	}
	if err := validateEnvString(key, value); err != nil {
		return err
	}
	if key == "MC_METRICS_DIRECTORIES" {
		directories, err := parseMetricsDirectories(value)
		if err == nil {
			c.MetricsDirectories = directories
		}
		return err
	}
	if key == "MC_LAUNCH_ARGS" {
		args, err := parseEnvLaunchArgs(value)
		if err == nil {
			c.LaunchArgs = args
		}
		return err
	}
	if target := f.strings[key]; target != nil {
		*target = value
		return nil
	}
	if target := f.durations[key]; target != nil {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("must use valid Go duration syntax")
		}
		*target = Duration(duration)
	}
	return nil
}

func parseEnvInteger(value string) (int, error) {
	if !decimalDigits(value) {
		return 0, fmt.Errorf("must be a positive decimal integer")
	}
	// Leading zeroes are accepted identically by the Go and Python readers.
	digits := strings.TrimLeft(value, "0")
	if digits == "" {
		digits = "0"
	}
	n, err := strconv.ParseInt(digits, 10, strconv.IntSize)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("integer must be positive and within range")
	}
	return int(n), nil
}

func parseEnvLaunchArgs(value string) ([]string, error) {
	var encoded []json.RawMessage
	if err := json.Unmarshal([]byte(value), &encoded); err != nil || encoded == nil || len(encoded) > maxLaunchArgs || !validJSONSurrogates(value) {
		return nil, fmt.Errorf("must be a JSON array containing at most 256 valid Unicode strings")
	}
	args := make([]string, len(encoded))
	for i, raw := range encoded {
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &args[i]) != nil {
			return nil, fmt.Errorf("every array item must be a string")
		}
	}
	return args, nil
}
