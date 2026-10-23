package telemetry

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"time"
)

type label struct{ Name, Value string }
type point struct {
	Name, Family, Help, Kind string
	Value                    float64
	Labels                   []label
}

func gauge(name, help string, value float64, labels ...string) point {
	p := point{Name: name, Family: name, Help: help, Kind: "gauge", Value: value}
	for i := 0; i+1 < len(labels); i += 2 {
		p.Labels = append(p.Labels, label{labels[i], labels[i+1]})
	}
	sort.Slice(p.Labels, func(i, j int) bool { return p.Labels[i].Name < p.Labels[j].Name })
	return p
}
func counter(name, help string, value float64, labels ...string) point {
	p := gauge(name, help, value, labels...)
	p.Kind = "counter"
	return p
}

func escapeHelp(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n").Replace(strings.ToValidUTF8(s, "�"))
}
func escapeLabel(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(strings.ToValidUTF8(s, "�"))
}
func labelsText(labels []label) string {
	if len(labels) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(l.Name)
		out.WriteString("=\"")
		out.WriteString(escapeLabel(l.Value))
		out.WriteByte('"')
	}
	out.WriteByte('}')
	return out.String()
}

// encode emits Prometheus0.0.4 text, grouping each family under one HELP/TYPE
// declaration and escaping the three reserved label characters explicitly.
func encode(points []point) []byte {
	families := make(map[string][]point)
	for _, p := range points {
		families[p.Family] = append(families[p.Family], p)
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, name := range names {
		family := families[name]
		sort.SliceStable(family, func(i, j int) bool {
			a, b := family[i], family[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if a.Kind == "histogram" && strings.HasSuffix(a.Name, "_bucket") {
				var x, y float64
				for _, l := range a.Labels {
					if l.Name == "le" {
						x, _ = strconv.ParseFloat(l.Value, 64)
					}
				}
				for _, l := range b.Labels {
					if l.Name == "le" {
						y, _ = strconv.ParseFloat(l.Value, 64)
					}
				}
				if x != y {
					return x < y
				}
			}
			return labelsText(a.Labels) < labelsText(b.Labels)
		})
		out.WriteString("# HELP " + name + " " + escapeHelp(family[0].Help) + "\n# TYPE " + name + " " + family[0].Kind + "\n")
		for _, p := range family {
			out.WriteString(p.Name)
			out.WriteString(labelsText(p.Labels))
			out.WriteByte(' ')
			out.WriteString(strconv.FormatFloat(p.Value, 'g', -1, 64))
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func boolean(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
func indexLabel(n int) string  { return strconv.Itoa(n) }
