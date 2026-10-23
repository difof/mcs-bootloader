package backend

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/difof/mcs-bootloader/internal/mcproto"
)

var servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service$`)

func (c Config) Validate() error {
	ports, err := c.validateNetwork()
	if err != nil {
		return err
	}
	if !servicePattern.MatchString(c.Service) {
		return fmt.Errorf("invalid backend service name")
	}
	if err := c.validatePaths(); err != nil {
		return err
	}
	if err := c.validateLaunch(); err != nil {
		return err
	}
	if !mcproto.SupportedProtocol(c.Protocol) {
		return fmt.Errorf("unsupported MC_PROTOCOL; supported: %s", mcproto.SupportedVersions)
	}
	if c.MaxConnections < 1 || c.MaxConnections > 4096 || c.MaxPlayers < 1 {
		return fmt.Errorf("invalid connection/player limit")
	}
	if err := c.validateMetricsListen(ports); err != nil {
		return err
	}
	if err := c.validateTimers(); err != nil {
		return err
	}
	if err := ValidateMetricsDirectories(c.MetricsDirectories); err != nil {
		return err
	}
	if c.PollInterval > c.IdleTimeout {
		return fmt.Errorf("poll_interval must not exceed idle_timeout")
	}
	if c.LogLevel != "info" && c.LogLevel != "debug" && c.LogLevel != "warn" {
		return fmt.Errorf("log_level must be info, warn, or debug")
	}
	return c.validateMessages()
}

func (c Config) validateNetwork() (map[int]bool, error) {
	_, listenPort, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen address must contain a valid host and port")
	}
	if !validPort(listenPort) {
		return nil, fmt.Errorf("invalid listen port")
	}
	listenNumber, _ := strconv.Atoi(listenPort)
	ports := map[int]bool{listenNumber: true}
	for _, address := range []string{c.BackendAddress, c.RCONAddress} {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("backend and RCON addresses must contain a valid host and port")
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("backend and RCON addresses must be numeric loopback addresses")
		}
		portNumber, _ := strconv.Atoi(port)
		if !validPort(port) || ports[portNumber] {
			return nil, fmt.Errorf("listen, backend, and RCON need distinct valid ports")
		}
		ports[portNumber] = true
	}
	return ports, nil
}

func (c Config) validatePaths() error {
	paths := []string{c.StateDirectory, c.AdminSocket, c.ServerDirectory}
	if c.RCONPassword != "" {
		if err := validatePassword(c.RCONPassword); err != nil {
			return err
		}
	} else {
		paths = append(paths, c.RCONPasswordFile)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return fmt.Errorf("runtime paths must be absolute and non-root")
		}
		if strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("runtime paths must not contain NUL or line breaks")
		}
	}
	return nil
}

func (c Config) validateMetricsListen(ports map[int]bool) error {
	metricsHost, metricsPort, metricsErr := net.SplitHostPort(c.MetricsListen)
	if metricsErr != nil || metricsHost == "" || !validPort(metricsPort) {
		return fmt.Errorf("metrics_listen must contain a numeric IP address and valid port")
	}
	if _, err := netip.ParseAddr(metricsHost); err != nil {
		return fmt.Errorf("metrics_listen must contain a numeric IP address")
	}
	metricsNumber, _ := strconv.Atoi(metricsPort)
	if c.MetricsEnabled && ports[metricsNumber] {
		return fmt.Errorf("enabled metrics must use a different port from the gateway, backend and RCON")
	}
	return nil
}

func (c Config) validateTimers() error {
	for name, d := range map[string]Duration{"idle_timeout": c.IdleTimeout, "poll_interval": c.PollInterval, "operation_timeout": c.OperationTimeout, "startup_timeout": c.StartupTimeout, "shutdown_warn_after": c.ShutdownWarnAfter, "wake_cooldown": c.WakeCooldown, "handshake_timeout": c.HandshakeTimeout, "dial_timeout": c.DialTimeout, "restart_grace": c.RestartGrace, "metrics_poll_interval": c.MetricsPollInterval, "metrics_player_interval": c.MetricsPlayerInterval, "metrics_directory_interval": c.MetricsDirectoryInterval, "metrics_directory_timeout": c.MetricsDirectoryTimeout} {
		if d <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	return nil
}

func (c Config) validateMessages() error {
	for _, message := range []string{c.BootMessage, c.SleepingMOTD, c.StartingMOTD, c.StoppingMOTD, c.UnknownMOTD, c.FailedMOTD, c.MOTD, c.RestartMessage, c.WhitelistMessage} {
		if len(message) == 0 || len(message) > 4096 || !utf8.ValidString(message) || strings.ContainsAny(message, "\x00\r") {
			return fmt.Errorf("messages must contain 1-4096 bytes")
		}
	}
	if strings.Contains(c.RestartMessage, "\n") {
		return fmt.Errorf("restart_message must be a single line")
	}
	return nil
}

const maxLaunchArgs = 256
const maxLaunchBytes = 32768

func (c Config) validateLaunch() error {
	if strings.TrimSpace(c.LaunchExecutable) == "" || len(c.LaunchExecutable) > 4096 ||
		strings.ContainsAny(c.LaunchExecutable, "\x00\r\n") || !utf8.ValidString(c.LaunchExecutable) {
		return fmt.Errorf("MC_LAUNCH_EXECUTABLE must be a nonempty valid UTF-8 path of at most 4096 bytes without NUL or line breaks")
	}
	if c.LaunchArgs == nil || len(c.LaunchArgs) > maxLaunchArgs {
		return fmt.Errorf("MC_LAUNCH_ARGS must be a JSON array containing at most 256 strings")
	}
	total := 0
	for _, arg := range c.LaunchArgs {
		total += len(arg)
		if strings.ContainsAny(arg, "\x00\r\n") || !utf8.ValidString(arg) {
			return fmt.Errorf("MC_LAUNCH_ARGS strings must contain valid UTF-8 without NUL or line breaks")
		}
	}
	if total > maxLaunchBytes {
		return fmt.Errorf("MC_LAUNCH_ARGS must contain at most 32768 UTF-8 bytes in total")
	}
	return nil
}

func validPort(s string) bool {
	n, e := strconv.Atoi(s)
	return decimalDigits(s) && e == nil && n > 0 && n <= 65535
}

// Directory labels are operator-chosen names, never arbitrary discovered paths.
// The count/path limits bound configuration and Prometheus label cardinality.
var metricsDirectoryName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

func ValidateMetricsDirectories(directories map[string]string) error {
	if len(directories) > 32 {
		return fmt.Errorf("MC_METRICS_DIRECTORIES supports at most 32 named directories")
	}
	for name, path := range directories {
		if !metricsDirectoryName.MatchString(name) {
			return fmt.Errorf("MC_METRICS_DIRECTORIES names must start with a letter and contain at most 64 ASCII letters, digits, dots, underscores or hyphens")
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" || len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("MC_METRICS_DIRECTORIES values must be absolute non-root paths of at most 4096 UTF-8 bytes without NUL or line breaks")
		}
	}
	return nil
}
