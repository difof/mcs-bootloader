package main

import (
	"errors"
	"github.com/difof/mcs-bootloader/internal/backend"
	"strconv"
)

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
	pid, err := parseBackendPID(o.PID)
	if err != nil {
		return err
	}
	if o.Command == "backend-stop" && pid == 0 {
		return nil
	}
	cfg, err := loadBackendSettings(o)
	if err != nil {
		return err
	}
	if err := service.validateService(cfg.Service); err != nil {
		return err
	}
	if o.Command == "backend-start" {
		return backend.StartServer(cfg)
	}
	return gracefulBackendStop(cfg, pid)
}

func parseBackendPID(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid < 0 {
		return 0, errors.New("invalid backend PID")
	}
	return pid, nil
}

func loadBackendSettings(o cliOptions) (backend.Config, error) {
	path := o.Env
	if path == "" {
		path = defaultConfigPath
	}
	socket := o.Socket
	if socket == "" {
		socket = "/run/mcs-bootloader/control.sock"
	}
	cfg, err := appliedBackendConfig(socket)
	if err == nil {
		return cfg, nil
	}
	disk, diskErr := backend.LoadConfig(path)
	if diskErr != nil {
		return cfg, errors.New("applied configuration is unavailable and .env could not be validated")
	}
	if disk.AdminSocket != socket {
		cfg, err = appliedBackendConfig(disk.AdminSocket)
	}
	if err == nil {
		return cfg, nil
	}
	if o.Command == "backend-start" {
		return cfg, errors.New("gateway applied settings are unavailable; start the gateway before Minecraft")
	}
	return disk, nil
}
