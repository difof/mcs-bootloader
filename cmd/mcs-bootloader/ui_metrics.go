package main

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

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
			families = appendMetricMetadata(families, indexes, line)
			continue
		}
		sample, err := parseMetricSample(line)
		if err != nil {
			return nil, nil, err
		}
		samples = append(samples, sample)
	}
	return samples, families, scanner.Err()
}

func appendMetricMetadata(families []metricFamily, indexes map[string]int, line string) []metricFamily {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) != 4 || (parts[1] != "HELP" && parts[1] != "TYPE") {
		return families
	}
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
	return families
}

func parseMetricSample(line string) (metricSample, error) {
	nameEnd := strings.IndexAny(line, "{ \t")
	if nameEnd < 1 {
		return metricSample{}, fmt.Errorf("invalid metrics sample")
	}
	sample := metricSample{Name: line[:nameEnd]}
	if !metricName.MatchString(sample.Name) {
		return sample, fmt.Errorf("invalid metrics name")
	}
	rest := strings.TrimSpace(line[nameEnd:])
	if strings.HasPrefix(rest, "{") {
		labels, consumed, err := parseMetricLabels(rest)
		if err != nil {
			return sample, err
		}
		sample.Labels = labels
		rest = strings.TrimSpace(rest[consumed:])
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 {
		return sample, fmt.Errorf("invalid metrics value")
	}
	number, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return sample, fmt.Errorf("invalid metrics number")
	}
	if math.IsInf(number, 0) || math.IsNaN(number) {
		sample.Value = fields[0]
	} else {
		sample.Value = number
	}
	return sample, nil
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
