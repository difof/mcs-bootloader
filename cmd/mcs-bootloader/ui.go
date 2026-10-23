package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

type cliOutput struct {
	Out, Err io.Writer
	JSON     bool
	Color    string
}

func newCLIOutput(out, err io.Writer) *cliOutput {
	return &cliOutput{Out: out, Err: err, Color: "auto"}
}
func encodeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}
func (o *cliOutput) color(w io.Writer, code, text string) string {
	if o.JSON || o.Color == "never" {
		return text
	}
	enabled := o.Color == "always"
	if o.Color == "auto" || o.Color == "" {
		_, noColor := os.LookupEnv("NO_COLOR")
		if f, ok := w.(*os.File); ok && !noColor && os.Getenv("TERM") != "dumb" {
			if s, e := f.Stat(); e == nil && s.Mode()&os.ModeCharDevice != 0 {
				enabled = true
			}
		}
	}
	if enabled {
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	return text
}
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x1b {
			return '\u241b'
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '\ufffd'
		}
		return r
	}, text)
}
func titleWords(text string) string {
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(text, "_", " "), "-", " "))
	for i, word := range words {
		if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}
func (o *cliOutput) Error(err error) {
	if o.JSON {
		_ = encodeJSON(o.Err, map[string]any{"error": err.Error()})
		return
	}
	fmt.Fprintln(o.Err, o.color(o.Err, "1;31", "Error:"), cleanText(err.Error()))
}
func (o *cliOutput) Result(title string, value any) error {
	if o.JSON {
		return encodeJSON(o.Out, value)
	}
	if _, err := fmt.Fprintln(o.Out, o.color(o.Out, "1;36", title)); err != nil {
		return err
	}
	writer := tabwriter.NewWriter(o.Out, 0, 4, 2, ' ', 0)
	renderValue(writer, "", value)
	return writer.Flush()
}
func renderValue(w io.Writer, prefix string, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		// Operational state deserves the first row, before ancillary metadata.
		for _, key := range []string{"state", "message", "version"} {
			for i, k := range keys {
				if k == key {
					keys = append([]string{k}, append(keys[:i], keys[i+1:]...)...)
					break
				}
			}
		}
		for _, key := range keys {
			label := titleWords(key)
			if strings.HasPrefix(key, "MC_") {
				label = key
			}
			switch child := v[key].(type) {
			case map[string]any:
				fmt.Fprintf(w, "%s%s\n", prefix, label)
				renderValue(w, prefix+"  ", child)
			default:
				fmt.Fprintf(w, "%s%s\t%s\n", prefix, label, humanValue(child))
			}
		}
	default:
		fmt.Fprintf(w, "%s%s\n", prefix, humanValue(v))
	}
}
func humanValue(value any) string {
	if value == nil {
		return "unknown"
	}
	switch v := value.(type) {
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case string:
		if v == "" {
			return "—"
		}
		if stamp, err := time.Parse(time.RFC3339Nano, v); err == nil {
			if stamp.IsZero() {
				return "—"
			}
			return stamp.Local().Format("2006-01-02 15:04:05 MST")
		}
		return cleanText(v)
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatFloat(v, 'f', 0, 64)
		}
		return strconv.FormatFloat(v, 'g', 6, 64)
	case json.Number:
		return string(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "unavailable"
		}
		return cleanText(string(raw))
	}
}
func (o *cliOutput) CommandReply(reply string) error {
	if o.JSON {
		return encodeJSON(o.Out, map[string]any{"reply": reply})
	}
	_, err := fmt.Fprintln(o.Out, cleanText(strings.TrimSuffix(reply, "\n")))
	return err
}

type metricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  any               `json:"value"`
}
type metricFamily struct {
	Name string `json:"name"`
	Help string `json:"help,omitempty"`
	Type string `json:"type,omitempty"`
}

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func parseMetrics(data []byte) ([]metricSample, []metricFamily, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	samples := make([]metricSample, 0)
	families := make([]metricFamily, 0)
	indexes := map[string]int{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, " ", 4)
			if len(parts) == 4 && (parts[1] == "HELP" || parts[1] == "TYPE") {
				name := parts[2]
				index, ok := indexes[name]
				if !ok {
					index = len(families)
					indexes[name] = index
					families = append(families, metricFamily{Name: name})
				}
				if parts[1] == "HELP" {
					families[index].Help = parts[3]
				} else {
					families[index].Type = parts[3]
				}
			}
			continue
		}
		nameEnd := strings.IndexAny(line, "{ \t")
		if nameEnd < 1 {
			return nil, nil, fmt.Errorf("invalid metrics sample")
		}
		sample := metricSample{Name: line[:nameEnd]}
		if !metricName.MatchString(sample.Name) {
			return nil, nil, fmt.Errorf("invalid metrics name")
		}
		rest := strings.TrimSpace(line[nameEnd:])
		if strings.HasPrefix(rest, "{") {
			labels, consumed, err := parseMetricLabels(rest)
			if err != nil {
				return nil, nil, err
			}
			sample.Labels = labels
			rest = strings.TrimSpace(rest[consumed:])
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 || len(fields) > 2 {
			return nil, nil, fmt.Errorf("invalid metrics value")
		}
		number, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid metrics number")
		}
		if math.IsInf(number, 0) || math.IsNaN(number) {
			sample.Value = fields[0]
		} else {
			sample.Value = number
		}
		samples = append(samples, sample)
	}
	return samples, families, scanner.Err()
}
func parseMetricLabels(text string) (map[string]string, int, error) {
	labels := make(map[string]string)
	i := 1
	for i < len(text) {
		if text[i] == '}' {
			return labels, i + 1, nil
		}
		start := i
		for i < len(text) && text[i] != '=' {
			i++
		}
		if i == len(text) {
			break
		}
		name := text[start:i]
		if !metricName.MatchString(name) {
			break
		}
		i++
		if i >= len(text) || text[i] != '"' {
			break
		}
		start = i
		i++
		for i < len(text) {
			if text[i] == '\\' {
				i += 2
				continue
			}
			if text[i] == '"' {
				break
			}
			i++
		}
		if i >= len(text) {
			break
		}
		value, err := strconv.Unquote(text[start : i+1])
		if err != nil {
			return nil, 0, fmt.Errorf("invalid metrics label")
		}
		labels[name] = value
		i++
		if i < len(text) && text[i] == ',' {
			i++
			continue
		}
		if i < len(text) && text[i] == '}' {
			return labels, i + 1, nil
		}
		break
	}
	return nil, 0, fmt.Errorf("invalid metrics labels")
}
func (o *cliOutput) Metrics(raw []byte, rawMode bool) error {
	if rawMode {
		_, err := o.Out.Write(raw)
		return err
	}
	samples, families, err := parseMetrics(raw)
	if err != nil {
		return err
	}
	if o.JSON {
		return encodeJSON(o.Out, map[string]any{"families": families, "samples": samples})
	}
	fmt.Fprintln(o.Out, o.color(o.Out, "1;36", "Prometheus metrics"))
	fmt.Fprintf(o.Out, "%d metric families · %d samples\n", len(families), len(samples))
	writer := tabwriter.NewWriter(o.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "METRIC\tVALUE\tLABELS")
	for _, sample := range samples {
		labels := make([]string, 0, len(sample.Labels))
		for key, value := range sample.Labels {
			labels = append(labels, key+"="+strconv.Quote(value))
		}
		sort.Strings(labels)
		fmt.Fprintf(writer, "%s\t%s\t%s\n", sample.Name, humanValue(sample.Value), strings.Join(labels, ", "))
	}
	return writer.Flush()
}

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
