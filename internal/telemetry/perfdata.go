package telemetry

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxPerfData = 4 << 20

// The version2 layout, units and variability values follow OpenJDK21's
// src/hotspot/share/runtime/{perfMemory.hpp,perfData.hpp}. The magic is always
// big endian; all subsequent integers follow the declared byte order.
type perfNumber struct {
	Value              int64
	Units, Variability byte
}
type perfData struct {
	Numbers        map[string]perfNumber
	Strings        map[string]string
	Used, Overflow int
}

var safePerfName = regexp.MustCompile(`^(?:java|sun|com\.sun)\.[A-Za-z0-9_.]+$`)
var spaceName = regexp.MustCompile(`^sun\.gc\.generation\.([0-9]+)\.space\.([0-9]+)\.(used|capacity|maxCapacity|initCapacity)$`)
var generationName = regexp.MustCompile(`^sun\.gc\.generation\.([0-9]+)\.(capacity|minCapacity|maxCapacity)$`)
var collectorName = regexp.MustCompile(`^sun\.gc\.collector\.([0-9]+)\.(invocations|time)$`)

func parsePerfData(data []byte) (perfData, error) {
	p := perfData{Numbers: make(map[string]perfNumber), Strings: make(map[string]string)}
	if len(data) < 32 || len(data) > maxPerfData || binary.BigEndian.Uint32(data[:4]) != 0xcafec0c0 {
		return p, errors.New("invalid HotSpot perfdata header")
	}
	if data[4] > 1 || data[5] != 2 || data[7] != 1 {
		return p, errors.New("HotSpot perfdata is unsupported or not accessible")
	}
	var order binary.ByteOrder = binary.BigEndian
	if data[4] == 1 {
		order = binary.LittleEndian
	}
	used := int(order.Uint32(data[8:12]))
	overflow := int(order.Uint32(data[12:16]))
	offset := int(order.Uint32(data[24:28]))
	count := int(order.Uint32(data[28:32]))
	if used < 32 || used > len(data) || offset < 32 || offset > used || count < 0 || count > 8192 {
		return p, errors.New("HotSpot perfdata bounds are invalid")
	}
	p.Used, p.Overflow = used, overflow
	seen := make(map[string]bool)
	for i := 0; i < count; i++ {
		if offset > used-20 {
			return p, errors.New("truncated HotSpot perfdata entry")
		}
		h := data[offset : offset+20]
		length := int(order.Uint32(h[0:4]))
		nameOffset := int(order.Uint32(h[4:8]))
		vector := int(order.Uint32(h[8:12]))
		valueOffset := int(order.Uint32(h[16:20]))
		if length < 20 || length > used-offset || nameOffset < 20 || nameOffset >= length || valueOffset < 20 || valueOffset >= length || vector < 0 {
			return p, errors.New("invalid HotSpot perfdata entry bounds")
		}
		entry := data[offset : offset+length]
		if vector > 0 {
			width := 1
			switch h[12] {
			case 'J', 'D':
				width = 8
			case 'I', 'F':
				width = 4
			case 'S', 'C':
				width = 2
			}
			if vector > (length-valueOffset)/width {
				return p, errors.New("HotSpot vector exceeds entry bounds")
			}
		}
		nameEnd := bytes.IndexByte(entry[nameOffset:], 0)
		if nameEnd < 1 || nameEnd > 512 {
			return p, errors.New("invalid HotSpot counter name")
		}
		name := string(entry[nameOffset : nameOffset+nameEnd])
		if seen[name] {
			return p, errors.New("duplicate HotSpot counter name")
		}
		seen[name] = true
		if safePerfName.MatchString(name) {
			switch {
			case h[12] == 'J' && vector == 0:
				if valueOffset > length-8 {
					return p, errors.New("truncated HotSpot scalar")
				}
				if h[14] < 1 || h[14] > 6 || h[15] < 1 || h[15] > 3 {
					return p, errors.New("invalid HotSpot counter metadata")
				}
				p.Numbers[name] = perfNumber{Value: int64(order.Uint64(entry[valueOffset : valueOffset+8])), Units: h[14], Variability: h[15]}
			case h[12] == 'B' && vector > 0 && h[14] == 5:
				if vector > length-valueOffset {
					return p, errors.New("truncated HotSpot string")
				}
				// Never retain arbitrary string counters. They may contain JVM
				// arguments, paths, class names or other private application data.
				if safePerfString(name) {
					raw := entry[valueOffset : valueOffset+vector]
					end := bytes.IndexByte(raw, 0)
					if end < 0 || end > 256 || !utf8.Valid(raw[:end]) {
						return p, errors.New("invalid HotSpot display name")
					}
					value := string(raw[:end])
					if strings.ContainsAny(value, "\r\n\x00") {
						return p, errors.New("invalid HotSpot display name")
					}
					p.Strings[name] = value
				}
			}
		}
		offset += length
	}
	return p, nil
}

func safePerfString(name string) bool {
	if name == "java.property.java.vm.version" || name == "java.property.java.vm.name" || name == "java.property.java.vm.vendor" {
		return true
	}
	return strings.HasPrefix(name, "sun.gc.") && strings.HasSuffix(name, ".name")
}

func readPerfData(root string, pid int, proc processData) (perfData, error) {
	// Resolve the target's own /tmp through procfs. This works with a private
	// /tmp on either systemd unit without joining its namespace or using sudo.
	dir := procPath(root, pid, "root/tmp")
	f, err := os.Open(dir)
	if err != nil {
		return perfData{}, err
	}
	entries, err := f.ReadDir(4097)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return perfData{}, err
	}
	if len(entries) > 4096 {
		return perfData{}, errors.New("target temporary directory exceeds scan limit")
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "hsperfdata_") {
			candidates = append(candidates, filepath.Join(dir, entry.Name(), strconv.Itoa(pid)))
		}
	}
	if len(candidates) > 128 {
		return perfData{}, errors.New("too many HotSpot perfdata directories")
	}
	sort.Strings(candidates)
	var result perfData
	found := false
	for _, path := range candidates {
		data, err := readBounded(path, maxPerfData)
		// Other users may also have private hsperfdata directories in /tmp.
		// An inaccessible unrelated directory must not hide our readable VM.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return perfData{}, err
		}
		if found {
			return perfData{}, errors.New("ambiguous HotSpot perfdata PID")
		}
		result, err = parsePerfData(data)
		if err != nil {
			return perfData{}, err
		}
		// A second bounded header read detects structural changes during the
		// snapshot. Values themselves are naturally sampled at nearby times.
		again, err := readBounded(path, maxPerfData)
		if err != nil {
			return perfData{}, err
		}
		if len(again) < 32 || !bytes.Equal(data[:32], again[:32]) {
			return perfData{}, errors.New("HotSpot perfdata structure changed during read")
		}
		created, ok := result.Numbers["sun.rt.createVmBeginTime"]
		if !ok || math.Abs(float64(created.Value)/1000-proc.StartTime) > 30 {
			return perfData{}, errors.New("HotSpot perfdata belongs to a different process start")
		}
		found = true
	}
	if !found {
		return perfData{}, errors.New("HotSpot shared perfdata is unavailable")
	}
	start, err := processIdentity(root, pid)
	if err != nil {
		return perfData{}, err
	}
	if start != proc.StartTicks {
		return perfData{}, errors.New("Java process identity changed during perfdata read")
	}
	return result, nil
}

func (p perfData) points() []point {
	points := []point{gauge("mcs_jvm_perfdata_used_bytes", "Bytes used in the HotSpot shared perfdata buffer.", float64(p.Used)), gauge("mcs_jvm_perfdata_overflow_bytes", "HotSpot counters that overflowed the shared perfdata buffer, in bytes.", float64(p.Overflow)), gauge("mcs_jvm_perfdata_counters", "Numeric HotSpot counters exported.", float64(len(p.Numbers)))}
	freq := float64(p.Numbers["sun.os.hrt.frequency"].Value)
	for name, n := range p.Numbers {
		unit := map[byte]string{1: "none", 2: "bytes", 3: "ticks", 4: "events", 5: "string", 6: "hertz"}[n.Units]
		variability := map[byte]string{1: "constant", 2: "monotonic", 3: "variable"}[n.Variability]
		points = append(points, gauge("mcs_jvm_perfdata_value", "Numeric HotSpot counter in its original declared unit; internal names vary by JVM.", float64(n.Value), "counter", name, "unit", unit, "variability", variability))
		if match := spaceName.FindStringSubmatch(name); len(match) == 4 {
			base := "sun.gc.generation." + match[1]
			field := map[string]string{"used": "used", "capacity": "committed", "maxCapacity": "max", "initCapacity": "initial"}[match[3]]
			points = append(points, gauge("mcs_jvm_memory_pool_"+field+"_bytes", "HotSpot heap space memory accounting.", float64(n.Value), "generation", match[1], "generation_name", p.Strings[base+".name"], "pool", match[2], "pool_name", p.Strings[base+".space."+match[2]+".name"]))
		}
		if match := generationName.FindStringSubmatch(name); len(match) == 3 {
			field := map[string]string{"capacity": "committed", "minCapacity": "min", "maxCapacity": "max"}[match[2]]
			points = append(points, gauge("mcs_jvm_generation_"+field+"_bytes", "HotSpot heap generation capacity; generation maxima must not be summed.", float64(n.Value), "generation", match[1], "generation_name", p.Strings["sun.gc.generation."+match[1]+".name"]))
		}
		if match := collectorName.FindStringSubmatch(name); len(match) == 3 {
			labels := []string{"collector", match[1], "collector_name", p.Strings["sun.gc.collector."+match[1]+".name"]}
			if match[2] == "invocations" {
				points = append(points, counter("mcs_jvm_gc_collections_total", "HotSpot collector invocations.", float64(n.Value), labels...))
			} else if freq > 0 {
				points = append(points, counter("mcs_jvm_gc_duration_seconds_total", "Time reported by each HotSpot collector, converted using the JVM clock frequency.", float64(n.Value)/freq, labels...))
			}
		}
	}
	for _, space := range []string{"metaspace", "compressedclassspace"} {
		for suffix, metric := range map[string]string{"used": "used", "capacity": "committed", "maxCapacity": "max", "minCapacity": "min"} {
			if n, ok := p.Numbers["sun.gc."+space+"."+suffix]; ok {
				points = append(points, gauge("mcs_jvm_nonheap_"+metric+"_bytes", "HotSpot non-heap memory accounting.", float64(n.Value), "pool", space))
			}
		}
	}
	for _, entry := range []struct {
		key, name, help string
		counter         bool
	}{
		{"java.threads.live", "mcs_jvm_threads_live", "Live Java threads.", false},
		{"java.threads.daemon", "mcs_jvm_threads_daemon", "Daemon Java threads.", false},
		{"java.threads.livePeak", "mcs_jvm_threads_peak", "Peak live Java threads.", false},
		{"java.threads.started", "mcs_jvm_threads_started_total", "Java threads started since VM startup.", true},
		{"java.cls.loadedClasses", "mcs_jvm_classes_loaded_total", "Classes loaded since VM startup.", true},
		{"java.cls.unloadedClasses", "mcs_jvm_classes_unloaded_total", "Classes unloaded since VM startup.", true},
		{"sun.rt.safepoints", "mcs_jvm_safepoints_total", "HotSpot safepoint operations.", true},
		{"sun.ci.totalCompiles", "mcs_jvm_jit_compilations_total", "HotSpot completed JIT compilations.", true},
		{"sun.ci.totalBailouts", "mcs_jvm_jit_bailouts_total", "HotSpot JIT compilation bailouts.", true},
	} {
		if n, ok := p.Numbers[entry.key]; ok {
			v := gauge(entry.name, entry.help, float64(n.Value))
			if entry.counter {
				v.Kind = "counter"
			}
			points = append(points, v)
		}
	}
	if loaded, ok := p.Numbers["java.cls.loadedClasses"]; ok {
		if unloaded, ok := p.Numbers["java.cls.unloadedClasses"]; ok {
			points = append(points, gauge("mcs_jvm_classes_currently_loaded", "Currently loaded classes.", float64(loaded.Value-unloaded.Value)))
		}
	}
	if freq > 0 {
		for key, name := range map[string]string{"sun.rt.safepointTime": "safepoint", "sun.rt.safepointSyncTime": "safepoint_sync", "sun.rt.applicationTime": "application", "java.ci.totalTime": "jit_compilation"} {
			if n, ok := p.Numbers[key]; ok {
				points = append(points, counter("mcs_jvm_"+name+"_seconds_total", "Accumulated HotSpot time converted using JVM clock frequency.", float64(n.Value)/freq))
			}
		}
	}
	if version := p.Strings["java.property.java.vm.version"]; version != "" {
		points = append(points, gauge("mcs_jvm_info", "Java VM implementation information from selected safe perfdata fields.", 1, "version", version, "name", p.Strings["java.property.java.vm.name"], "vendor", p.Strings["java.property.java.vm.vendor"]))
	}
	return points
}
