package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var logUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service$`)
var logNamespacePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func runLogs(ctx context.Context, o cliOptions, out *cliOutput, original []string) error {
	path, err := resolveEnv(o.Env, false)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return reexecAs(ctx, "root", withEnvArgument(original, path), out)
	}
	values, err := readOperatorValues(path)
	if err != nil {
		return err
	}
	args, err := journalArguments(o, values)
	if err != nil {
		return err
	}
	return readJournal(ctx, args, values["MC_RCON_PASSWORD"], out)
}

func journalArguments(o cliOptions, values map[string]string) ([]string, error) {
	unit := values["MC_GATEWAY_SERVICE"]
	if unit == "" {
		unit = "mcs-bootloader.service"
	}
	if o.Server {
		unit = values["MC_BACKEND_SERVICE"]
		if unit == "" {
			unit = "mcs-bootloader-backend.service"
		}
	}
	namespace := values["MC_LOG_NAMESPACE"]
	if namespace == "" {
		namespace = "mcs-bootloader"
	}
	if !logUnitPattern.MatchString(unit) || !logNamespacePattern.MatchString(namespace) {
		return nil, errors.New("invalid journal unit or namespace in .env")
	}
	lines, err := journalLines(o.Lines, values["MC_LOG_LINES"])
	if err != nil {
		return nil, err
	}
	args := []string{"--namespace=" + namespace, "--unit=" + unit, "--lines=" + strconv.Itoa(lines), "--output=json", "--no-pager", "--output-fields=__REALTIME_TIMESTAMP,MESSAGE,PRIORITY,_SYSTEMD_UNIT"}
	if o.Follow {
		args = append(args, "--follow")
	}
	return args, nil
}

func journalLines(explicit int, configured string) (int, error) {
	if explicit != 0 {
		return explicit, nil
	}
	if configured == "" {
		return 100, nil
	}
	lines, err := strconv.Atoi(configured)
	if err != nil || lines < 1 || lines > 100000 {
		return 0, errors.New("MC_LOG_LINES must be between 1 and 100000")
	}
	return lines, nil
}

func readJournal(ctx context.Context, args []string, password string, out *cliOutput) error {
	command := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var diagnostic boundedText
	command.Stderr = &diagnostic
	if err = command.Start(); err != nil {
		return fmt.Errorf("start journal reader: %w", err)
	}
	renderErr := renderJournal(stdout, password, out)
	if renderErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if renderErr != nil {
		return renderErr
	}
	if waitErr != nil {
		return fmt.Errorf("journal reader failed: %s", cleanText(diagnostic.String()))
	}
	return nil
}

func renderJournal(reader io.Reader, password string, out *cliOutput) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		record, err := journalRecord(scanner.Bytes(), password)
		if err != nil {
			return err
		}
		if err = out.Log(record); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type boundedText struct{ strings.Builder }

func (b *boundedText) Write(data []byte) (int, error) {
	n := len(data)
	remaining := 8192 - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.Builder.Write(data)
	}
	return n, nil
}

func journalRecord(data []byte, password string) (map[string]any, error) {
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, errors.New("journal returned an invalid JSON record")
	}
	message, ok := entry["MESSAGE"].(string)
	if !ok {
		message = "[binary journal message omitted]"
	}
	result := map[string]any{}
	if raw, ok := entry["__REALTIME_TIMESTAMP"].(string); ok {
		if micros, err := strconv.ParseInt(raw, 10, 64); err == nil {
			result["time"] = time.UnixMicro(micros).UTC().Format(time.RFC3339Nano)
		}
	}
	if unit, ok := entry["_SYSTEMD_UNIT"].(string); ok {
		result["unit"] = unit
	}
	priority, _ := entry["PRIORITY"].(string)
	level := "INFO"
	switch priority {
	case "0", "1", "2", "3":
		level = "ERROR"
	case "4":
		level = "WARN"
	case "7":
		level = "DEBUG"
	}
	result["level"] = level
	var structured map[string]any
	if json.Unmarshal([]byte(message), &structured) == nil && structured != nil {
		if _, present := structured["msg"]; present {
			for key, value := range structured {
				result[key] = value
			}
		} else {
			result["message"] = message
		}
	} else {
		result["message"] = message
	}
	redactRecord(result, password)
	return result, nil
}
func redactRecord(record map[string]any, password string) {
	if password == "" {
		return
	}
	var redact func(any) any
	redact = func(value any) any {
		switch v := value.(type) {
		case string:
			return strings.ReplaceAll(v, password, "[REDACTED]")
		case map[string]any:
			for k, item := range v {
				v[k] = redact(item)
			}
		case []any:
			for i, item := range v {
				v[i] = redact(item)
			}
		}
		return value
	}
	for key, value := range record {
		record[key] = redact(value)
	}
}
