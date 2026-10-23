package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

type Config struct {
	Enabled                                        bool
	Listen                                         string
	PollInterval, OperationTimeout, PlayerInterval time.Duration
	PlayerDetails                                  bool
	ServerDirectory                                string
	Directories                                    map[string]string
	DirectoryInterval, DirectoryTimeout            time.Duration
}

type Snapshot struct {
	Lifecycle        control.Snapshot
	Traffic          gateway.Stats
	IdleTimeout      time.Duration
	ConfigGeneration uint64
}

type Source struct {
	Snapshot  func() Snapshot
	Command   func(context.Context, string, time.Duration) (string, error)
	PID       func() int
	RCONStats func() rcon.Stats
	Status    func() json.RawMessage
}

func (cfg Config) validate() error {
	if cfg.DirectoryInterval <= 0 || cfg.DirectoryTimeout <= 0 {
		return errors.New("directory metrics intervals must be positive")
	}
	if err := backend.ValidateMetricsDirectories(cfg.Directories); err != nil {
		return err
	}
	if cfg.PollInterval <= 0 || cfg.OperationTimeout <= 0 || cfg.PlayerInterval <= 0 {
		return errors.New("metrics intervals must be positive")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return errors.New("metrics listener must be a numeric IP and port")
	}
	if _, err = netip.ParseAddr(host); err != nil {
		return errors.New("metrics listener must use a numeric IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("metrics listener port must be between 1 and 65535")
	}
	return nil
}
