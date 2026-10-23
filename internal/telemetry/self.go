package telemetry

import (
	"os"
	"runtime"
)

func (m *Manager) selfPoints() ([]point, error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	points := []point{
		gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine())),
		gauge("go_threads", "Number of OS threads created by the Go runtime.", float64(runtimeThreads())),
		gauge("go_info", "Go runtime information.", 1, "version", runtime.Version()),
		gauge("go_gomaxprocs", "Maximum Go threads executing user code simultaneously.", float64(runtime.GOMAXPROCS(0))),
		gauge("go_memstats_alloc_bytes", "Bytes allocated and still in use.", float64(mem.Alloc)),
		counter("go_memstats_alloc_bytes_total", "Total bytes allocated, including freed objects.", float64(mem.TotalAlloc)),
		gauge("go_memstats_sys_bytes", "Bytes obtained from the system.", float64(mem.Sys)),
		counter("go_memstats_mallocs_total", "Total heap objects allocated.", float64(mem.Mallocs)),
		counter("go_memstats_frees_total", "Total heap objects freed.", float64(mem.Frees)),
		gauge("go_memstats_heap_alloc_bytes", "Heap bytes allocated and still in use.", float64(mem.HeapAlloc)),
		gauge("go_memstats_heap_sys_bytes", "Heap bytes obtained from the system.", float64(mem.HeapSys)),
		gauge("go_memstats_heap_idle_bytes", "Heap bytes in idle spans.", float64(mem.HeapIdle)),
		gauge("go_memstats_heap_inuse_bytes", "Heap bytes in in-use spans.", float64(mem.HeapInuse)),
		gauge("go_memstats_heap_released_bytes", "Heap bytes released to the operating system.", float64(mem.HeapReleased)),
		gauge("go_memstats_heap_objects", "Currently allocated heap objects.", float64(mem.HeapObjects)),
		gauge("go_memstats_stack_inuse_bytes", "Bytes in stack spans.", float64(mem.StackInuse)),
		gauge("go_memstats_stack_sys_bytes", "Stack bytes obtained from the system.", float64(mem.StackSys)),
		gauge("go_memstats_mspan_inuse_bytes", "Bytes of allocated span metadata.", float64(mem.MSpanInuse)),
		gauge("go_memstats_mcache_inuse_bytes", "Bytes of allocated cache metadata.", float64(mem.MCacheInuse)),
		gauge("go_memstats_gc_sys_bytes", "Bytes used for GC metadata.", float64(mem.GCSys)),
		gauge("go_memstats_next_gc_bytes", "Heap target for the next GC cycle.", float64(mem.NextGC)),
		counter("go_gc_cycles_total", "Completed garbage collection cycles.", float64(mem.NumGC)),
		counter("go_gc_forced_cycles_total", "Forced garbage collection cycles.", float64(mem.NumForcedGC)),
		counter("go_gc_pause_seconds_total", "Total stop-the-world GC pause time.", float64(mem.PauseTotalNs)/1e9),
		gauge("go_gc_cpu_fraction", "Fraction of available CPU time used by GC since startup.", mem.GCCPUFraction),
	}
	if mem.LastGC > 0 {
		points = append(points, gauge("go_gc_last_timestamp_seconds", "Unix time of the last completed GC.", float64(mem.LastGC)/1e9))
	}
	proc, err := readProcess("/proc", os.Getpid(), m.clockTicks, m.bootTime)
	points = append(points, gauge("process_metrics_available", "Whether Linux process accounting was readable during the latest self collection.", boolean(err == nil)))
	if err != nil {
		return points, nil
	} // Go metrics remain available on non-Linux hosts.
	points = append(points, proc.points("process")...)
	return points, nil
}

func runtimeThreads() int { n, _ := runtime.ThreadCreateProfile(nil); return n }
