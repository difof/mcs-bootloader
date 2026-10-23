package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		return errors.New("invalid journal unit or namespace in .env")
	}
	lines := o.Lines
	if lines == 0 {
		lines = 100
		if text := values["MC_LOG_LINES"]; text != "" {
			lines, err = strconv.Atoi(text)
			if err != nil || lines < 1 || lines > 100000 {
				return errors.New("MC_LOG_LINES must be between 1 and 100000")
			}
		}
	}
	args := []string{"--namespace=" + namespace, "--unit=" + unit, "--lines=" + strconv.Itoa(lines), "--output=json", "--no-pager", "--output-fields=__REALTIME_TIMESTAMP,MESSAGE,PRIORITY,_SYSTEMD_UNIT"}
	if o.Follow {
		args = append(args, "--follow")
	}
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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var renderErr error
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		record, err := journalRecord(scanner.Bytes(), values["MC_RCON_PASSWORD"])
		if err != nil {
			renderErr = err
			break
		}
		if err = out.Log(record); err != nil {
			renderErr = err
			break
		}
	}
	if renderErr == nil {
		renderErr = scanner.Err()
	}
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
