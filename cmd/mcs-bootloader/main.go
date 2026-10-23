// mcs-bootloader is a Minecraft TCP gateway with conservative lifecycle control.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := newCLIOutput(os.Stdout, os.Stderr)
	err := executeCLI(ctx, os.Args[1:], out)
	if err == nil {
		return
	}
	var child *childExitError
	if errors.As(err, &child) {
		os.Exit(child.Code)
	}
	out.Error(err)
	os.Exit(1)
}

// run is retained for the backend helper integration tests and embedders.
func run(args []string) error {
	return executeCLI(context.Background(), args, newCLIOutput(os.Stdout, os.Stderr))
}

type childExitError struct{ Code int }

func (e *childExitError) Error() string { return fmt.Sprintf("child exited with status %d", e.Code) }

func writeJSON(w io.Writer, value any) error { return encodeJSON(w, value) }
