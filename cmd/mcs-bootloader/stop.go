package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/rcon"
)

// systemd invokes this helper before stopping the backend. It cannot escalate to
// signals or a force kill. The unit's stop timeout is infinite and SIGKILL disabled.
func gracefulBackendStop(cfg backend.Config, pid int) error {
	// MAINPID can be empty when an RCON-requested clean exit precedes ExecStop.
	if pid == 0 {
		return nil
	}
	if pid <= 1 {
		return fmt.Errorf("backend-stop requires a valid --pid")
	}
	identity, err := processIdentity(pid)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	password, err := cfg.Password()
	if err != nil {
		return err
	}
	client := rcon.New(cfg.RCONAddress, password, time.Duration(cfg.OperationTimeout))
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("backend_stop_requested", "pid", pid)
	attempted := false
	var lastWarning time.Time
	for {
		current, err := processIdentity(pid)
		if os.IsNotExist(err) || err == nil && current != identity {
			log.Info("backend_exited", "pid", pid)
			return nil
		}
		if err != nil {
			return err
		}
		if !attempted {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.OperationTimeout))
			_, err := client.Players(ctx)
			cancel()
			if err != nil {
				if time.Since(lastWarning) > time.Minute {
					lastWarning = time.Now()
					log.Warn("backend_stop_waiting_for_rcon", "error", err.Error())
				}
			} else {
				// A lost reply does not prove the command was not executed. Never
				// repeat an uncertain stop while the server may still be saving.
				attempted = true
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.OperationTimeout))
				err := client.Stop(ctx)
				cancel()
				if err == nil {
					log.Info("backend_stop_acknowledged")
				} else {
					log.Warn("backend_stop_ack_unknown", "error", err.Error(), "action", "waiting_for_process_exit_without_resending")
				}
			}
		}
		time.Sleep(time.Second)
	}
}

func processIdentity(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// Field 2 can contain spaces or parentheses; remaining fields start after the
	// final ')'. Field 22 is the kernel start time, protecting against PID reuse.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process metadata")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("incomplete process metadata")
	}
	if fields[0] == "Z" {
		return "", os.ErrNotExist
	}
	return fields[19], nil
}
