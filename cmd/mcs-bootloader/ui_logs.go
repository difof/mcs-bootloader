package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// logWriter receives one bounded JSON slog record at a time. Systemd uses
// structured records; foreground serve uses the same renderer as `logs`.
type logWriter struct{ output *cliOutput }

func (w logWriter) Write(data []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	err := w.output.Log(record)
	return len(data), err
}
func (o *cliOutput) Log(record map[string]any) error {
	if o.JSON {
		return encodeJSON(o.Out, record)
	}
	timestamp, _ := record["time"].(string)
	if t, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
		timestamp = t.Local().Format("2006-01-02 15:04:05")
	}
	level, _ := record["level"].(string)
	if level == "" {
		level = "INFO"
	}
	message, _ := record["message"].(string)
	if message == "" {
		message, _ = record["msg"].(string)
	}
	code := "36"
	if level == "WARN" {
		code = "33"
	}
	if level == "ERROR" {
		code = "31"
	}
	if level == "DEBUG" {
		code = "90"
	}
	prefix := strings.TrimSpace(timestamp + " " + o.color(o.Out, code, fmt.Sprintf("%-5s", level)))
	extra := make([]string, 0)
	for key, value := range record {
		if key != "time" && key != "level" && key != "message" && key != "msg" {
			extra = append(extra, key+"="+humanValue(value))
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		message += "  " + strings.Join(extra, " ")
	}
	_, err := fmt.Fprintf(o.Out, "%s  %s\n", prefix, cleanText(message))
	return err
}
