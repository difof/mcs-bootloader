package gateway

import (
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/difof/mcs-bootloader/internal/mcproto"
)

type Config struct {
	BackendAddress   string
	ServerDirectory  string
	Protocol         int
	MaxConnections   int
	HandshakeTimeout time.Duration
	DialTimeout      time.Duration
	BootMessage      string
	SleepingMOTD     string
	StartingMOTD     string
	StoppingMOTD     string
	UnknownMOTD      string
	FailedMOTD       string
	OverrideMOTD     bool
	MOTD             string
	RestartMessage   string
	WhitelistMessage string
	FallbackStatus   json.RawMessage
}

// Validate checks a complete configuration without applying defaults. The
// startup-only backend address and protocol are additionally pinned by Server.
func (cfg Config) Validate() error {
	host, port, err := net.SplitHostPort(cfg.BackendAddress)
	if err != nil || host == "" {
		return errors.New("backend address must contain a host and port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("backend port must be between 1 and 65535")
	}
	if !mcproto.SupportedProtocol(cfg.Protocol) {
		return errors.New("unsupported protocol; supported: " + mcproto.SupportedVersions)
	}
	if !filepath.IsAbs(cfg.ServerDirectory) || filepath.Clean(cfg.ServerDirectory) == "/" || strings.ContainsAny(cfg.ServerDirectory, "\x00\r\n") {
		return errors.New("server directory must be an absolute, non-root path")
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > 4096 {
		return errors.New("maximum connections must be between 1 and 4096")
	}
	if cfg.HandshakeTimeout <= 0 || cfg.DialTimeout <= 0 {
		return errors.New("handshake and dial timeouts must be positive")
	}
	for _, message := range []string{cfg.BootMessage, cfg.SleepingMOTD, cfg.StartingMOTD, cfg.StoppingMOTD, cfg.UnknownMOTD, cfg.FailedMOTD, cfg.MOTD, cfg.RestartMessage, cfg.WhitelistMessage} {
		if len(message) == 0 || len(message) > 4096 || !utf8.ValidString(message) || strings.ContainsAny(message, "\x00\r") {
			return errors.New("messages must contain 1-4096 bytes")
		}
	}
	if strings.Contains(cfg.RestartMessage, "\n") {
		return errors.New("restart message must be a single line")
	}
	if len(cfg.FallbackStatus) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cfg.FallbackStatus, &obj); err != nil || obj == nil {
			return errors.New("fallback status must be a JSON object")
		}
	}
	return nil
}

func (cfg *Config) defaults() {
	if cfg.ServerDirectory == "" {
		cfg.ServerDirectory = "/srv/minecraft"
	}
	if cfg.Protocol == 0 {
		cfg.Protocol = 767
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 64
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 8 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	cfg.defaultMessages()
}

func (cfg *Config) defaultMessages() {
	for field, fallback := range map[*string]string{
		&cfg.BootMessage:      "Server is booting up. {eta} Please reconnect.",
		&cfg.SleepingMOTD:     "Sleeping - join to start the server",
		&cfg.StartingMOTD:     "Server is starting. Please reconnect shortly.",
		&cfg.FailedMOTD:       "Server is temporarily unavailable. Please try again shortly.",
		&cfg.StoppingMOTD:     "Server is saving and going to sleep. Please reconnect shortly.",
		&cfg.UnknownMOTD:      "Checking server health. Please reconnect shortly.",
		&cfg.MOTD:             "Minecraft server",
		&cfg.RestartMessage:   "Gateway is restarting. Please reconnect in a moment.",
		&cfg.WhitelistMessage: "You are not whitelisted on this server.",
	} {
		if *field == "" {
			*field = fallback
		}
	}
}
