// mc-gateway is a Minecraft TCP gateway with conservative server lifecycle control.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"mc-gateway/internal/backend"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mc-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "command" {
		return runCommand(args, os.Stdout)
	}
	if command == "status" || command == "wake" || command == "sleep" || command == "reload" || command == "config" || command == "restart-check" {
		return adminClient(command, args)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	pidText := flags.String("pid", "", "backend main PID (backend-stop only)")
	socket := flags.String("socket", "/run/mc-gateway/control.sock", "applied backend configuration socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if command == "backend-start" || command == "backend-stop" {
		pid := 0
		if *pidText != "" {
			var err error
			pid, err = strconv.Atoi(*pidText)
			if err != nil {
				return fmt.Errorf("invalid backend PID")
			}
		}
		if command == "backend-stop" && pid == 0 {
			return nil
		}
		// Consult the running daemon before parsing pending on-disk edits.
		// --socket lets an installation pin a nondefault control endpoint.
		cfg, err := appliedBackendConfig(*socket)
		if err != nil {
			disk, diskErr := backend.LoadConfig(*configPath)
			if diskErr != nil {
				return fmt.Errorf("applied configuration is unavailable and .env could not be validated")
			}
			if disk.AdminSocket != *socket {
				cfg, err = appliedBackendConfig(disk.AdminSocket)
			}
			if err != nil {
				if command == "backend-start" {
					return fmt.Errorf("gateway applied settings are unavailable; start the gateway before Minecraft")
				}
				// Offline recovery retains the same no-force-stop policy.
				cfg = disk
			}
		}
		if command == "backend-start" {
			return backend.StartJava(cfg)
		}
		return gracefulBackendStop(cfg, pid)
	}
	if command == "serve" {
		return serve(*configPath)
	}
	cfg, err := backend.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	switch command {
	case "check":
		if _, err := cfg.Password(); err != nil {
			return err
		}
		fmt.Println("configuration and password permissions are valid")
		return nil
	default:
		return fmt.Errorf("unknown command %q; use serve, check, status, config, reload, restart-check, wake, sleep, command, backend-start, backend-stop, or version", command)
	}
}
