package control

import (
	"errors"
	"fmt"
	"time"
)

type Config struct {
	IdleTimeout       time.Duration
	PollInterval      time.Duration
	OperationTimeout  time.Duration
	StartupTimeout    time.Duration
	ShutdownWarnAfter time.Duration
	WakeCooldown      time.Duration
}

// Validate checks a complete runtime configuration. Reconfiguration never
// substitutes defaults for missing values, so a bad reload cannot silently
// shorten a safety timeout.
func (cfg Config) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"idle timeout", cfg.IdleTimeout},
		{"poll interval", cfg.PollInterval},
		{"operation timeout", cfg.OperationTimeout},
		{"startup timeout", cfg.StartupTimeout},
		{"shutdown warning interval", cfg.ShutdownWarnAfter},
		{"wake cooldown", cfg.WakeCooldown},
	} {
		if field.value <= 0 {
			return fmt.Errorf("%s must be positive", field.name)
		}
	}
	if cfg.PollInterval > cfg.IdleTimeout {
		return errors.New("poll interval must not exceed idle timeout")
	}
	return nil
}

func (cfg Config) withDefaults() Config {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 15 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 5 * time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 15 * time.Minute
	}
	if cfg.ShutdownWarnAfter <= 0 {
		cfg.ShutdownWarnAfter = 5 * time.Minute
	}
	if cfg.WakeCooldown <= 0 {
		cfg.WakeCooldown = 30 * time.Second
	}
	return cfg
}
