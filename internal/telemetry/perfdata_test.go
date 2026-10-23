package telemetry

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type perfFixtureEntry struct {
	Name               string
	Number             int64
	Text               string
	Units, Variability byte
	String             bool
}

func perfFixture(order binary.ByteOrder, entries ...perfFixtureEntry) []byte {
	data := make([]byte, 32)
	binary.BigEndian.PutUint32(data, 0xcafec0c0)
	if order == binary.LittleEndian {
		data[4] = 1
	}
	data[5] = 2
	data[7] = 1
	for _, e := range entries {
		name := append([]byte(e.Name), 0)
		valueOffset := (20 + len(name) + 7) &^ 7
		var value []byte
		if e.String {
			value = append([]byte(e.Text), 0)
		} else {
			value = make([]byte, 8)
			order.PutUint64(value, uint64(e.Number))
		}
		length := (valueOffset + len(value) + 7) &^ 7
		buf := make([]byte, length)
		order.PutUint32(buf, uint32(length))
		order.PutUint32(buf[4:], 20)
		buf[12] = 'J'
		buf[14] = e.Units
		buf[15] = e.Variability
		order.PutUint32(buf[16:], uint32(valueOffset))
		if e.String {
			buf[12] = 'B'
			buf[14] = 5
			order.PutUint32(buf[8:], uint32(len(value)))
		}
		copy(buf[20:], name)
		copy(buf[valueOffset:], value)
		data = append(data, buf...)
	}
	order.PutUint32(data[8:], uint32(len(data)))
	order.PutUint32(data[24:], 32)
	order.PutUint32(data[28:], uint32(len(entries)))
	return data
}

func TestPerfDataEndianUnitsAndSecretExclusion(t *testing.T) {
	entries := []perfFixtureEntry{
		{Name: "sun.os.hrt.frequency", Number: 1000000000, Units: 6, Variability: 1},
		{Name: "sun.gc.collector.0.time", Number: 1500000000, Units: 3, Variability: 2},
		{Name: "sun.gc.collector.0.invocations", Number: 9, Units: 4, Variability: 2},
		{Name: "sun.gc.collector.0.name", Text: "G1 young collection pauses", String: true, Variability: 1},
		{Name: "sun.gc.generation.0.space.0.used", Number: 123456, Units: 2, Variability: 3},
		{Name: "sun.gc.generation.0.space.0.name", Text: "eden", String: true, Variability: 1},
		{Name: "sun.rt.jvmArgs", Text: "-Drcon.password=do-not-export-secret", String: true, Variability: 1},
		{Name: "sun.rt.javaCommand", Text: "server.jar secret-argument", String: true, Variability: 1},
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		p, err := parsePerfData(perfFixture(order, entries...))
		if err != nil {
			t.Fatal(err)
		}
		body := string(encode(p.points()))
		if strings.Contains(body, "secret") || strings.Contains(body, "jvmArgs") || strings.Contains(body, "javaCommand") {
			t.Fatal("unsafe perf string reached metrics")
		}
		if !strings.Contains(body, "mcs_jvm_gc_duration_seconds_total{collector=\"0\",collector_name=\"G1 young collection pauses\"} 1.5\n") {
			t.Fatal("incorrect JVM time conversion:", body)
		}
		if !strings.Contains(body, "unit=\"bytes\"") {
			t.Fatal("missing raw counter unit")
		}
	}
}

func TestPerfDataMalformedBoundsNeverPanic(t *testing.T) {
	data := perfFixture(binary.LittleEndian, perfFixtureEntry{Name: "java.threads.live", Number: 78, Units: 1, Variability: 3})
	for end := 0; end < len(data); end++ {
		if _, err := parsePerfData(data[:end]); err == nil {
			t.Errorf("accepted truncated buffer of size %d", end)
		}
	}
	for _, offset := range []int{4, 5, 7, 8, 24, 28, 32, 36, 40, 48} {
		bad := append([]byte{}, data...)
		bad[offset] = 0xff
		if _, err := parsePerfData(bad); err == nil {
			t.Errorf("accepted malformed offset %d", offset)
		}
	}
	dup := perfFixture(binary.LittleEndian, perfFixtureEntry{Name: "java.threads.live", Units: 1, Variability: 3}, perfFixtureEntry{Name: "java.threads.live", Units: 1, Variability: 3})
	if _, err := parsePerfData(dup); err == nil {
		t.Fatal("accepted duplicate perf counters")
	}
}

func procStatFixture(pid int, start uint64) []byte {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = "250"
	fields[12] = "50"
	fields[17] = "8"
	fields[19] = strconv.FormatUint(start, 10)
	fields[20] = "1048576"
	fields[21] = "123"
	return []byte(strconv.Itoa(pid) + " (java (worker)) " + strings.Join(fields, " ") + "\n")
}

func TestProcessStatCommandParenthesesAndClockUnits(t *testing.T) {
	p, err := parseProcessStat(procStatFixture(123, 500), 123, 250, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if p.CPUSeconds != 1.2 || p.StartTime != 1002 || p.Threads != 8 || p.ResidentBytes != float64(123*os.Getpagesize()) {
		t.Fatalf("incorrect proc interpretation %+v", p)
	}
	if _, err = parseProcessStat(procStatFixture(123, 500), 456, 250, 1000); err == nil {
		t.Fatal("accepted wrong PID")
	}
	if _, err = parseProcessStat(procStatFixture(123, 500), 123, 0, 1000); err == nil {
		t.Fatal("assumed hardcoded clock frequency")
	}
}

func TestPerfDataRejectsStalePIDAndStartIdentity(t *testing.T) {
	root := t.TempDir()
	const pid = 123
	dir := filepath.Join(root, "123", "root", "tmp", "hsperfdata_test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "123", "root", "tmp", "hsperfdata_aaa_unrelated")
	if err := os.Mkdir(other, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(other, 0700)
	stat := filepath.Join(root, "123", "stat")
	if err := os.WriteFile(stat, procStatFixture(pid, 500), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "123")
	fixture := func(created int64) []byte {
		return perfFixture(binary.LittleEndian, perfFixtureEntry{Name: "sun.rt.createVmBeginTime", Number: created, Units: 1, Variability: 3})
	}
	if err := os.WriteFile(path, fixture(1002000), 0600); err != nil {
		t.Fatal(err)
	}
	proc := processData{PID: pid, StartTicks: 500, StartTime: 1002}
	if _, err := readPerfData(root, pid, proc); err != nil {
		t.Fatal("valid namespace perfdata:", err)
	}
	if err := os.WriteFile(path, fixture(2002000), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPerfData(root, pid, proc); err == nil {
		t.Fatal("accepted perfdata from another JVM startup")
	}
	if err := os.WriteFile(path, fixture(1002000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stat, procStatFixture(pid, 501), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPerfData(root, pid, proc); err == nil {
		t.Fatal("accepted recycled PID identity")
	}
}

func TestReadBoundedRejectsFIFOAndSymlinkWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readBounded(fifo, 100); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("metrics file read blocked on FIFO")
	}
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(real, 4); err == nil {
		t.Fatal("accepted oversized file")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(link, 100); err == nil {
		t.Fatal("followed final symlink")
	}
}
