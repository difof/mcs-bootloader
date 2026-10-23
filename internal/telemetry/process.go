package telemetry

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxProcFile = 2 << 20

type processData struct {
	PID                                                         int
	StartTicks                                                  uint64
	CPUSeconds, StartTime, VirtualBytes, ResidentBytes, Threads float64
	OpenFDs, MaxFDs                                             *float64
	IO                                                          map[string]float64
	Status                                                      map[string]float64
}

// Linux USER_HZ is obtained from libc, not assumed to be 100. This command is
// executed once when metrics are first enabled, never by HTTP scrapes.
func processClock(parent context.Context) (float64, float64) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "getconf", "CLK_TCK").Output()
	if err != nil {
		return 0, 0
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil || !finite(hz) || hz <= 0 {
		return 0, 0
	}
	stat, err := readBounded("/proc/stat", maxProcFile)
	if err != nil {
		return hz, 0
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if strings.HasPrefix(line, "btime ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 64)
			if err == nil && finite(v) && v > 0 {
				return hz, v
			}
		}
	}
	return hz, 0
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("metrics source is not a regular file")
	}
	if st.Size() > max {
		return nil, errors.New("metrics source exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("metrics source exceeds read limit")
	}
	return data, nil
}

func procPath(root string, pid int, file string) string {
	return filepath.Join(root, strconv.Itoa(pid), file)
}

// The comm field may contain spaces and closing parentheses; only its final
// closing parenthesis separates it from stat's documented numeric fields.
func parseProcessStat(data []byte, pid int, hz, boot float64) (processData, error) {
	p := processData{PID: pid, IO: make(map[string]float64), Status: make(map[string]float64)}
	text := string(data)
	left, right := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if left < 1 || right <= left {
		return p, errors.New("invalid proc stat command field")
	}
	actual, err := strconv.Atoi(strings.TrimSpace(text[:left]))
	if err != nil || actual != pid {
		return p, errors.New("proc stat PID mismatch")
	}
	fields := strings.Fields(text[right+1:])
	if len(fields) < 22 {
		return p, errors.New("truncated proc stat")
	}
	values := make(map[int]uint64)
	for _, i := range []int{11, 12, 17, 19, 20, 21} {
		n, e := strconv.ParseUint(fields[i], 10, 64)
		if e != nil {
			return p, errors.New("invalid proc stat numeric field")
		}
		values[i] = n
	}
	p.StartTicks = values[19]
	p.Threads = float64(values[17])
	p.VirtualBytes = float64(values[20])
	p.ResidentBytes = float64(values[21]) * float64(os.Getpagesize())
	if hz <= 0 || boot <= 0 {
		return p, errors.New("process clock frequency or boot time unavailable")
	}
	p.CPUSeconds = (float64(values[11]) + float64(values[12])) / hz
	p.StartTime = boot + float64(p.StartTicks)/hz
	return p, nil
}

func readProcess(root string, pid int, hz, boot float64) (processData, error) {
	if pid <= 0 {
		return processData{}, errors.New("invalid process PID")
	}
	data, err := readBounded(procPath(root, pid, "stat"), 65536)
	if err != nil {
		return processData{}, err
	}
	p, err := parseProcessStat(data, pid, hz, boot)
	if err != nil {
		return p, err
	}
	fd, err := os.Open(procPath(root, pid, "fd"))
	if err == nil {
		names, e := fd.Readdirnames(65537)
		_ = fd.Close()
		if (e == nil || errors.Is(e, io.EOF)) && len(names) <= 65536 {
			v := float64(len(names))
			p.OpenFDs = &v
		}
	}
	if data, err = readBounded(procPath(root, pid, "limits"), 65536); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Max open files") {
				parts := strings.Fields(strings.TrimPrefix(line, "Max open files"))
				if len(parts) > 0 {
					v, e := strconv.ParseFloat(parts[0], 64)
					if e == nil && finite(v) && v >= 0 {
						p.MaxFDs = &v
					}
				}
			}
		}
	}
	if data, err = readBounded(procPath(root, pid, "io"), 65536); err == nil {
		p.IO = parseKeyNumbers(data)
	}
	if data, err = readBounded(procPath(root, pid, "status"), 65536); err == nil {
		p.Status = parseKeyNumbers(data)
	}
	// Exclude torn samples after an exit or PID reuse during the reads.
	last, err := readBounded(procPath(root, pid, "stat"), 65536)
	if err != nil {
		return p, err
	}
	after, err := parseProcessStat(last, pid, hz, boot)
	if err != nil {
		return p, err
	}
	if after.StartTicks != p.StartTicks {
		return p, errors.New("process identity changed while sampling")
	}
	return p, nil
}

func parseKeyNumbers(data []byte) map[string]float64 {
	out := make(map[string]float64)
	s := bufio.NewScanner(strings.NewReader(string(data)))
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		parts := strings.Fields(value)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || !finite(v) {
			continue
		}
		if len(parts) > 1 && parts[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

func verifyJavaProcess(root string, pid int) error {
	exe, err := os.Readlink(procPath(root, pid, "exe"))
	if err != nil {
		return err
	}
	if filepath.Base(strings.TrimSuffix(exe, " (deleted)")) != "java" {
		return errors.New("backend main process is not Java")
	}
	return nil
}

func (p processData) points(prefix string) []point {
	points := []point{
		gauge(prefix+"_pid", "Sampled process identifier.", float64(p.PID)),
		counter(prefix+"_cpu_seconds_total", "Total user and system CPU time.", p.CPUSeconds),
		gauge(prefix+"_start_time_seconds", "Unix process start time from procfs and system clock ticks.", p.StartTime),
		gauge(prefix+"_resident_memory_bytes", "Resident memory from procfs.", p.ResidentBytes),
		gauge(prefix+"_virtual_memory_bytes", "Virtual memory from procfs.", p.VirtualBytes),
		gauge(prefix+"_threads", "Operating system threads in the sampled process.", p.Threads),
	}
	if p.OpenFDs != nil {
		points = append(points, gauge(prefix+"_open_fds", "Open file descriptors.", *p.OpenFDs))
	}
	if p.MaxFDs != nil {
		points = append(points, gauge(prefix+"_max_fds", "Soft limit on open file descriptors.", *p.MaxFDs))
	}
	for key, name := range map[string]string{"read_bytes": "read_bytes_total", "write_bytes": "written_bytes_total", "rchar": "read_characters_total", "wchar": "written_characters_total", "syscr": "read_calls_total", "syscw": "write_calls_total"} {
		if v, ok := p.IO[key]; ok {
			points = append(points, counter(prefix+"_io_"+name, "Process I/O accounting from procfs.", v))
		}
	}
	for key, name := range map[string]string{"VmSwap": "swap_bytes", "VmData": "data_memory_bytes", "VmStk": "stack_memory_bytes"} {
		if v, ok := p.Status[key]; ok {
			points = append(points, gauge(prefix+"_"+name, "Process memory accounting from procfs.", v))
		}
	}
	for key, kind := range map[string]string{"voluntary_ctxt_switches": "voluntary", "nonvoluntary_ctxt_switches": "involuntary"} {
		if v, ok := p.Status[key]; ok {
			points = append(points, counter(prefix+"_context_switches_total", "Process context switches from procfs.", v, "kind", kind))
		}
	}
	return points
}

func processIdentity(root string, pid int) (uint64, error) {
	data, err := readBounded(procPath(root, pid, "stat"), 65536)
	if err != nil {
		return 0, err
	}
	p, err := parseProcessStat(data, pid, 1, 1)
	if err != nil {
		return 0, fmt.Errorf("read process identity: %w", err)
	}
	return p.StartTicks, nil
}
