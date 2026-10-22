package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("duration must be a string such as 1h")
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

type Config struct {
	Listen            string   `json:"listen"`
	BackendAddress    string   `json:"backend_address"`
	RCONAddress       string   `json:"rcon_address"`
	RCONPasswordFile  string   `json:"rcon_password_file"`
	Service           string   `json:"backend_service"`
	StateDirectory    string   `json:"state_directory"`
	AdminSocket       string   `json:"admin_socket"`
	Protocol          int      `json:"protocol"`
	Version           string   `json:"version"`
	MaxPlayers        int      `json:"max_players"`
	MaxConnections    int      `json:"max_connections"`
	IdleTimeout       Duration `json:"idle_timeout"`
	PollInterval      Duration `json:"poll_interval"`
	OperationTimeout  Duration `json:"operation_timeout"`
	StartupTimeout    Duration `json:"startup_timeout"`
	ShutdownWarnAfter Duration `json:"shutdown_warn_after"`
	WakeCooldown      Duration `json:"wake_cooldown"`
	HandshakeTimeout  Duration `json:"handshake_timeout"`
	DialTimeout       Duration `json:"dial_timeout"`
	BootMessage       string   `json:"boot_message"`
	SleepingMOTD      string   `json:"sleeping_motd"`
	StartingMOTD      string   `json:"starting_motd"`
	FailedMOTD        string   `json:"failed_motd"`
	LogLevel          string   `json:"log_level"`
}

func DefaultConfig() Config {
	return Config{
		Listen: ":25565", BackendAddress: "127.0.0.1:25566", RCONAddress: "127.0.0.1:25575",
		RCONPasswordFile: "/etc/mc-gateway/rcon.password", Service: "mc-gateway-backend.service",
		StateDirectory: "/var/lib/mc-gateway", AdminSocket: "/run/mc-gateway/control.sock",
		Protocol: 767, Version: "1.21.1", MaxPlayers: 5, MaxConnections: 64,
		IdleTimeout: Duration(time.Hour), PollInterval: Duration(15 * time.Second), OperationTimeout: Duration(5 * time.Second),
		StartupTimeout: Duration(15 * time.Minute), ShutdownWarnAfter: Duration(3 * time.Minute), WakeCooldown: Duration(30 * time.Second),
		HandshakeTimeout: Duration(5 * time.Second), DialTimeout: Duration(3 * time.Second),
		BootMessage:  "Server is booting up. Please reconnect in a few minutes.",
		SleepingMOTD: "Server is sleeping. Join to start it.", StartingMOTD: "Server is booting up. Please wait...",
		FailedMOTD: "Server is unavailable. Please try again shortly.", LogLevel: "info",
	}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return c, err
	}
	if len(raw) > 65536 {
		return c, fmt.Errorf("configuration exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration has trailing data")
	}
	return c, c.Validate()
}

var servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service$`)

func (c Config) Validate() error {
	_, listenPort, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	if !validPort(listenPort) {
		return fmt.Errorf("invalid listen port")
	}
	ports := map[string]bool{listenPort: true}
	for _, address := range []string{c.BackendAddress, c.RCONAddress} {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("private address: %w", err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return fmt.Errorf("backend and RCON addresses must be numeric loopback addresses")
		}
		if !validPort(port) || ports[port] {
			return fmt.Errorf("listen, backend, and RCON need distinct valid ports")
		}
		ports[port] = true
	}
	if !servicePattern.MatchString(c.Service) {
		return fmt.Errorf("invalid backend service name")
	}
	for _, path := range []string{c.StateDirectory, c.AdminSocket, c.RCONPasswordFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return fmt.Errorf("runtime paths must be absolute and non-root")
		}
	}
	if c.Protocol != 767 {
		return fmt.Errorf("this release supports Minecraft 1.21.1 protocol 767; other versions require compatibility tests")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 4096 || c.MaxPlayers < 1 {
		return fmt.Errorf("invalid connection/player limit")
	}
	for name, d := range map[string]Duration{"idle_timeout": c.IdleTimeout, "poll_interval": c.PollInterval, "operation_timeout": c.OperationTimeout, "startup_timeout": c.StartupTimeout, "shutdown_warn_after": c.ShutdownWarnAfter, "wake_cooldown": c.WakeCooldown, "handshake_timeout": c.HandshakeTimeout, "dial_timeout": c.DialTimeout} {
		if d <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.PollInterval > c.IdleTimeout {
		return fmt.Errorf("poll_interval must not exceed idle_timeout")
	}
	if c.LogLevel != "info" && c.LogLevel != "debug" && c.LogLevel != "warn" {
		return fmt.Errorf("log_level must be info, warn, or debug")
	}
	for _, message := range []string{c.BootMessage, c.SleepingMOTD, c.StartingMOTD, c.FailedMOTD} {
		if len(message) == 0 || len(message) > 4096 {
			return fmt.Errorf("messages must contain 1-4096 bytes")
		}
	}
	return nil
}

func validPort(s string) bool { n, e := strconv.Atoi(s); return e == nil && n > 0 && n <= 65535 }

func ReadPassword(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open RCON password file: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0027 != 0 {
		return "", fmt.Errorf("RCON password file must be regular, not group-writable or accessible to other users (0600/0640)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(data))
	if len(password) < 16 || len(data) > 4096 || strings.ContainsAny(password, "\x00\r\n") {
		return "", fmt.Errorf("RCON password must be 16-4096 bytes without line breaks or NUL")
	}
	return password, nil
}
