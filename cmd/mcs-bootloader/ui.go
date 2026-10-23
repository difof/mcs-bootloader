package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
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
