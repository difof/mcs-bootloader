// Package operator runs the embedded administrative helpers. The gateway does
// not use Python at runtime; it is needed only for installation and maintenance.
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	assets "github.com/difof/mcs-bootloader"
)

// Options contains explicit operator paths. Neither path is inferred from the
// extraction directory, the executable's parent, or a checked-out repository.
type Options struct {
	Env        string
	Executable string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// The isolated interpreter ignores PYTHONPATH, PYTHONHOME, user-site modules,
// and sitecustomize. Only this private embedded directory is added to sys.path.
const bootstrap = `import runpy,sys; sys.path.insert(0,sys.argv.pop(1)); runpy.run_module("manage",run_name="__main__")`

const maxHelperOutput = 4 * 1024 * 1024

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxHelperOutput - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func extract(root string) error {
	return fs.WalkDir(assets.Files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if !fs.ValidPath(name) {
			return errors.New("invalid embedded asset path")
		}
		destination := filepath.Join(root, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.Mkdir(destination, 0700)
		}
		data, err := assets.Files.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0600)
	})
}

// Run returns exactly one JSON result. Privileged actions must be re-executed
// as root by the CLI before entering Run, so root never runs Python scripts
// unpacked into another user's writable temporary directory.
func Run(ctx context.Context, action string, args []string, opts Options) (json.RawMessage, error) {
	if opts.Env == "" {
		return nil, errors.New("an explicit .env path is required")
	}
	envPath, err := filepath.Abs(opts.Env)
	if err != nil {
		return nil, err
	}
	executable := opts.Executable
	if executable == "" {
		executable, err = os.Executable()
	}
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	python := "/usr/bin/python3"
	if os.Geteuid() != 0 {
		python, err = exec.LookPath("python3")
		if err != nil {
			return nil, errors.New("Python 3 is required for this operator command; install python3")
		}
	}
	if _, err = os.Stat(python); err != nil {
		return nil, errors.New("Python 3 is required for this operator command; install python3")
	}
	root, err := os.MkdirTemp("/tmp", "mcs-bootloader-assets-")
	if err != nil {
		return nil, fmt.Errorf("create private helper directory: %w", err)
	}
	defer os.RemoveAll(root)
	if err := extract(root); err != nil {
		return nil, fmt.Errorf("unpack embedded helpers: %w", err)
	}
	argv := []string{"-I", "-S", "-c", bootstrap, filepath.Join(root, "scripts"), "--embedded", "--env", envPath, "--executable", executable, action}
	argv = append(argv, args...)
	command := exec.CommandContext(ctx, python, argv...)
	command.Stdin = opts.Stdin
	interactive := action == "config-edit"
	if !interactive {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// Python translates SIGTERM into an exception so install transactions can
	// roll back. Signal its helper subprocess group as well.
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if interactive {
			return command.Process.Signal(syscall.SIGTERM)
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// Do not impose exec's forced-kill timer on a privileged transaction. The
	// helper handles SIGTERM and completes rollback before its assets disappear.
	var stdout, stderr boundedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("operator helper exceeded its output limit")
	}
	var envelope struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if json.Unmarshal(data, &envelope) == nil && envelope.OK != nil && !*envelope.OK {
		return nil, errors.New(envelope.Error)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2000 {
			detail = detail[:2000]
		}
		if detail != "" {
			return nil, fmt.Errorf("operator helper failed: %s", detail)
		}
		return nil, fmt.Errorf("operator helper failed: %w", err)
	}
	if !json.Valid(data) {
		return nil, errors.New("operator helper returned invalid structured output")
	}
	return append(json.RawMessage(nil), data...), nil
}
