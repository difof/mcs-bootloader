package backend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("duration must be a string such as 1h")
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("duration must use valid Go duration syntax")
	}
	*d = Duration(value)
	return nil
}

type Config struct {
	Listen           string `json:"listen"`
	BackendAddress   string `json:"backend_address"`
	RCONAddress      string `json:"rcon_address"`
	RCONPasswordFile string `json:"rcon_password_file"`
	// RCONPassword lives only in the private .env, never a JSON response or log.
	RCONPassword             string            `json:"-"`
	Service                  string            `json:"backend_service"`
	StateDirectory           string            `json:"state_directory"`
	AdminSocket              string            `json:"admin_socket"`
	ServerDirectory          string            `json:"server_directory"`
	LaunchExecutable         string            `json:"launch_executable"`
	LaunchArgs               []string          `json:"launch_args"`
	Protocol                 int               `json:"protocol"`
	Version                  string            `json:"version"`
	MaxPlayers               int               `json:"max_players"`
	MaxConnections           int               `json:"max_connections"`
	IdleTimeout              Duration          `json:"idle_timeout"`
	PollInterval             Duration          `json:"poll_interval"`
	OperationTimeout         Duration          `json:"operation_timeout"`
	StartupTimeout           Duration          `json:"startup_timeout"`
	ShutdownWarnAfter        Duration          `json:"shutdown_warn_after"`
	WakeCooldown             Duration          `json:"wake_cooldown"`
	HandshakeTimeout         Duration          `json:"handshake_timeout"`
	DialTimeout              Duration          `json:"dial_timeout"`
	RestartGrace             Duration          `json:"restart_grace"`
	BootMessage              string            `json:"boot_message"`
	SleepingMOTD             string            `json:"sleeping_motd"`
	StartingMOTD             string            `json:"starting_motd"`
	StoppingMOTD             string            `json:"stopping_motd"`
	UnknownMOTD              string            `json:"unknown_motd"`
	FailedMOTD               string            `json:"failed_motd"`
	OverrideMOTD             bool              `json:"override_motd"`
	MOTD                     string            `json:"motd"`
	RestartMessage           string            `json:"restart_message"`
	WhitelistMessage         string            `json:"whitelist_message"`
	MetricsEnabled           bool              `json:"metrics_enabled"`
	MetricsListen            string            `json:"metrics_listen"`
	MetricsPollInterval      Duration          `json:"metrics_poll_interval"`
	MetricsPlayerInterval    Duration          `json:"metrics_player_interval"`
	MetricsPlayerDetails     bool              `json:"metrics_player_details"`
	MetricsDirectories       map[string]string `json:"metrics_directories"`
	MetricsDirectoryInterval Duration          `json:"metrics_directory_interval"`
	MetricsDirectoryTimeout  Duration          `json:"metrics_directory_timeout"`
	LogLevel                 string            `json:"log_level"`
}

func DefaultConfig() Config {
	return Config{
		Listen: ":25565", BackendAddress: "127.0.0.1:25566", RCONAddress: "127.0.0.1:25575",
		RCONPasswordFile: "/etc/mcs-bootloader/rcon.password", Service: "mcs-bootloader-backend.service",
		StateDirectory: "/var/lib/mcs-bootloader", AdminSocket: "/run/mcs-bootloader/control.sock",
		ServerDirectory: "/srv/minecraft", LaunchExecutable: "/usr/bin/java", LaunchArgs: []string{"-jar", "server.jar", "nogui"},
		Protocol: 767, Version: "1.21.1", MaxPlayers: 5, MaxConnections: 64,
		IdleTimeout: Duration(15 * time.Minute), PollInterval: Duration(15 * time.Second), OperationTimeout: Duration(5 * time.Second),
		StartupTimeout: Duration(15 * time.Minute), ShutdownWarnAfter: Duration(3 * time.Minute), WakeCooldown: Duration(30 * time.Second),
		HandshakeTimeout: Duration(5 * time.Second), DialTimeout: Duration(3 * time.Second), RestartGrace: Duration(3 * time.Second),
		BootMessage:  "Server is booting up. {eta} Please reconnect.",
		SleepingMOTD: "Server is sleeping. Join to start it.", StartingMOTD: "Server is booting up. Please wait...",
		StoppingMOTD: "Server is saving and going to sleep. Please reconnect shortly.", UnknownMOTD: "Checking server health. Please reconnect shortly.",
		FailedMOTD: "Server is unavailable. Please try again shortly.", LogLevel: "info",
		MOTD: "Minecraft server", RestartMessage: "Gateway is restarting. Please reconnect in a moment.",
		WhitelistMessage: "You are not whitelisted on this server.",
		MetricsListen:    "127.0.0.1:9464", MetricsPollInterval: Duration(15 * time.Second), MetricsPlayerInterval: Duration(time.Minute), MetricsPlayerDetails: true,
		MetricsDirectories: map[string]string{}, MetricsDirectoryInterval: Duration(5 * time.Minute), MetricsDirectoryTimeout: Duration(30 * time.Second),
	}
}

// Clone keeps each applied configuration snapshot independent of callers.
func (c Config) Clone() Config {
	c.LaunchArgs = slices.Clone(c.LaunchArgs)
	c.MetricsDirectories = maps.Clone(c.MetricsDirectories)
	return c
}

// String and LogValue also guard against accidental logging with fmt or slog.
// The alias keeps this conversion independent of these formatting methods.
func (c Config) String() string {
	type publicConfig Config
	data, err := json.Marshal(publicConfig(c))
	if err != nil {
		return "<configuration unavailable>"
	}
	return string(data)
}

func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c Config) GoString() string { return c.String() }
