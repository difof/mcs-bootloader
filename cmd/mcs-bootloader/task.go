package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// taskArguments is compatibility for source contributors only. Task 3.44
// expands even CLI_ARGS_LIST as a template, so the wrapper deliberately uses
// its parent's original argv instead. No source helper, shell, or Task parser
// participates in ordinary release-binary commands.
func taskArguments(procRoot string, parentPID int) ([]string, error) {
	parent := filepath.Join(procRoot, strconv.Itoa(parentPID))
	executable, err := os.Readlink(filepath.Join(parent, "exe"))
	if err != nil {
		return nil, fmt.Errorf("inspect invoking Task: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(executable), " (deleted)")
	if name != "task" && name != "go-task" {
		return nil, errors.New("_task is a source wrapper: invoke task <task-name> -- <arguments>")
	}
	// /proc cmdline is not a regular file and has st_size zero; read it directly
	// with the same fixed upper bound used for all wrapper input.
	f, err := os.Open(filepath.Join(parent, "cmdline"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2*1024*1024 || len(data) == 0 || data[len(data)-1] != 0 {
		return nil, errors.New("invoking Task argument list is incomplete or too large")
	}
	argv := bytes.Split(data[:len(data)-1], []byte{0})
	for i, arg := range argv {
		if bytes.Equal(arg, []byte("--")) {
			result := make([]string, 0, len(argv)-i-1)
			for _, value := range argv[i+1:] {
				result = append(result, string(value))
			}
			return result, nil
		}
	}
	return nil, nil
}
