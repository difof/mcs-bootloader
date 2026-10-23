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
	if command == "status" || command == "wake" || command == "sleep" {
		return adminClient(command, args)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "/etc/mc-gateway/config.json", "configuration file")
	pidText := flags.String("pid", "", "backend main PID (backend-stop only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	cfg, err := backend.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	switch command {
	case "check":
		if _, err := backend.ReadPassword(cfg.RCONPasswordFile); err != nil {
			return err
		}
		fmt.Println("configuration and password permissions are valid")
		return nil
	case "serve":
		return serve(cfg)
	case "backend-stop":
		pid := 0
		if *pidText != "" {
			var err error
			pid, err = strconv.Atoi(*pidText)
			if err != nil {
				return fmt.Errorf("invalid backend PID")
			}
		}
		return gracefulBackendStop(cfg, pid)
	default:
		return fmt.Errorf("unknown command %q; use serve, check, status, wake, sleep, command, backend-stop, or version", command)
	}
}
