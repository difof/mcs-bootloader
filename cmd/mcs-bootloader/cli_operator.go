package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/difof/mcs-bootloader/internal/operator"
	"os"
	"path/filepath"
	"strings"
)

func selfExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func runOperator(ctx context.Context, o cliOptions, out *cliOutput, original []string) error {
	action := strings.ReplaceAll(o.Command, " ", "-")
	if action == "init" {
		action = "config-new"
	}
	if action == "config-init" {
		action = "config-link"
	}
	create := action == "config-new" || action == "config-link"
	if create && len(o.Args) > 0 {
		if len(o.Args) > 1 || o.Env != "" {
			return errors.New("provide one destination path, or --env PATH")
		}
		o.Env = o.Args[0]
	}
	path, err := resolveEnv(o.Env, create)
	if err != nil {
		return err
	}
	root := action == "install" || action == "deploy" || action == "remove" || strings.HasPrefix(action, "gateway-")
	if root && os.Geteuid() != 0 {
		return reexecAs(ctx, "root", withEnvArgument(original, path), out)
	}
	executable, err := selfExecutable()
	if err != nil {
		return err
	}
	var helperArgs []string
	if o.Report != "" {
		report, err := filepath.Abs(o.Report)
		if err != nil {
			return err
		}
		helperArgs = []string{"--report", report}
	}
	if o.DryRun {
		helperArgs = append(helperArgs, "--dry-run")
	}
	if o.Yes {
		helperArgs = append(helperArgs, "--yes")
	}
	result, err := operator.Run(ctx, action, helperArgs, operator.Options{Env: path, Executable: executable, Stdin: os.Stdin, Stdout: out.Out, Stderr: out.Err})
	if err != nil {
		return err
	}
	var data any
	if err = json.Unmarshal(result, &data); err != nil {
		return errors.New("operator returned an invalid structured result")
	}
	return out.Result(titleWords(o.Command), data)
}
