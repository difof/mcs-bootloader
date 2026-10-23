package telemetry

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Linux O_PATH obtains metadata without opening device drivers, sockets or
// FIFOs for I/O. All supported release targets are Linux. Openat + O_NOFOLLOW
// keeps traversal anchored to already-open directories, including during
// concurrent renames; no discovered path is ever re-opened from the root.
const directoryPathOnly = 0x200000
const maxDirectoryEntries = 1_000_000
const maxDirectoryDepth = 256

type directoryTotals struct {
	Bytes, Allocated, Files, Directories, Symlinks, Special, Mounts uint64
}

type directorySample struct {
	Path                     string
	Totals                   directoryTotals
	Success, InFlight        bool
	LastAttempt, LastSuccess time.Time
	Duration                 time.Duration
	Errors                   uint64
}

type fileIdentity struct{ device, inode uint64 }

type directoryWalk struct {
	ctx     context.Context
	device  uint64
	visited uint64
	seen    map[fileIdentity]bool
	totals  directoryTotals
}

// openDirectoryRoot refuses symlinks in every component, including the named
// root. The operator must configure the actual directory rather than an alias.
func openDirectoryRoot(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if part == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			current.Close()
			return nil, err
		}
		fd, err := syscall.Openat(int(current.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = os.NewFile(uintptr(fd), part)
	}
	return current, nil
}

func scanDirectory(ctx context.Context, path string) (directoryTotals, error) {
	dir, err := openDirectoryRoot(ctx, path)
	if err != nil {
		return directoryTotals{}, err
	}
	defer dir.Close()
	var stat syscall.Stat_t
	if err = syscall.Fstat(int(dir.Fd()), &stat); err != nil {
		return directoryTotals{}, err
	}
	w := directoryWalk{ctx: ctx, device: uint64(stat.Dev), seen: make(map[fileIdentity]bool)}
	err = w.walk(dir, stat, 0)
	if err != nil {
		return directoryTotals{}, err
	} // never publish partial totals
	return w.totals, nil
}

func (w *directoryWalk) walk(dir *os.File, stat syscall.Stat_t, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if depth > maxDirectoryDepth {
		return errors.New("directory nesting exceeds safety limit")
	}
	identity := fileIdentity{uint64(stat.Dev), stat.Ino}
	if w.seen[identity] {
		return nil
	}
	w.seen[identity] = true
	w.totals.Directories++
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(128)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			w.visited++
			if w.visited > maxDirectoryEntries {
				return errors.New("directory entry count exceeds safety limit")
			}
			// With O_PATH|O_NOFOLLOW a symlink is opened as the link itself. Other
			// nonregular files are also inspected without activating a device.
			fd, err := syscall.Openat(int(dir.Fd()), entry.Name(), directoryPathOnly|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			var child syscall.Stat_t
			err = syscall.Fstat(fd, &child)
			syscall.Close(fd)
			if err != nil {
				return err
			}
			if err = w.ctx.Err(); err != nil {
				return err
			}
			switch child.Mode & syscall.S_IFMT {
			case syscall.S_IFLNK:
				w.totals.Symlinks++
			case syscall.S_IFREG:
				id := fileIdentity{uint64(child.Dev), child.Ino}
				if w.seen[id] {
					continue
				}
				w.seen[id] = true
				if child.Size < 0 || child.Blocks < 0 {
					return errors.New("invalid file size")
				}
				bytes, blocks := uint64(child.Size), uint64(child.Blocks)
				if blocks > math.MaxUint64/512 || math.MaxUint64-w.totals.Bytes < bytes || math.MaxUint64-w.totals.Allocated < blocks*512 {
					return errors.New("directory size overflow")
				}
				w.totals.Files++
				w.totals.Bytes += bytes
				w.totals.Allocated += blocks * 512
			case syscall.S_IFDIR:
				if uint64(child.Dev) != w.device {
					w.totals.Mounts++
					continue
				}
				next, err := syscall.Openat(int(dir.Fd()), entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
				if err != nil {
					return err
				}
				opened := os.NewFile(uintptr(next), entry.Name())
				var check syscall.Stat_t
				err = syscall.Fstat(next, &check)
				if err == nil && (child.Dev != check.Dev || child.Ino != check.Ino) {
					err = errors.New("directory changed during scan")
				}
				if err == nil {
					err = w.walk(opened, check, depth+1)
				}
				opened.Close()
				if err != nil {
					return err
				}
			default:
				w.totals.Special++
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// runDirectories is separate from the regular metrics worker: a backup scan
// cannot delay player/JVM samples, lifecycle polling, socket requests or HTTP.
// There is at most one scan in flight; reload cancels it, never starts another
// concurrent traversal. OS filesystem calls can outlive cancellation on a hung
// mount, so Close does not wait for this read-only worker before exiting.
func (m *Manager) runDirectories() {
	defer close(m.directoryDone)
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.mu.RLock()
		cfg, generation := m.cfg, m.generation
		scan := m.directoryScan
		m.mu.RUnlock()
		if cfg.Enabled && len(cfg.Directories) > 0 {
			ctx, cancel := context.WithCancel(m.ctx)
			m.mu.Lock()
			if m.closed || m.ctx.Err() != nil {
				m.mu.Unlock()
				cancel()
				return
			}
			if generation != m.generation {
				m.mu.Unlock()
				cancel()
				continue
			}
			m.cancelDirectory = cancel
			m.mu.Unlock()
			names := make([]string, 0, len(cfg.Directories))
			for name := range cfg.Directories {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if ctx.Err() != nil {
					break
				}
				path := cfg.Directories[name]
				started := time.Now()
				m.mu.Lock()
				if generation != m.generation || m.closed {
					m.mu.Unlock()
					break
				}
				sample := m.directories[name]
				sample.Path = path
				sample.InFlight = true
				sample.LastAttempt = started
				m.directories[name] = sample
				m.mu.Unlock()
				budget, stop := context.WithTimeout(ctx, cfg.DirectoryTimeout)
				totals, err := scan(budget, path)
				if err == nil {
					err = budget.Err()
				}
				stop()
				if ctx.Err() != nil {
					break
				}
				sample.InFlight = false
				sample.Duration = time.Since(started)
				sample.Success = err == nil
				if err == nil {
					sample.Totals = totals
					sample.LastSuccess = time.Now()
				} else {
					sample.Errors++
				}
				m.mu.Lock()
				if generation == m.generation && !m.closed {
					m.directories[name] = sample
				}
				m.mu.Unlock()
			}
			m.mu.Lock()
			m.cancelDirectory = nil
			m.mu.Unlock()
			cancel()
		}
		interval := cfg.DirectoryInterval
		if interval <= 0 {
			interval = time.Hour
		}
		timer := time.NewTimer(interval)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-m.directoryWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Manager) directoryPoints(cfg Config) []point {
	m.mu.RLock()
	samples := make(map[string]directorySample, len(m.directories))
	for name, sample := range m.directories {
		samples[name] = sample
	}
	m.mu.RUnlock()
	points := []point{
		gauge("mcs_metrics_directory_interval_seconds", "Delay between completed directory scan cycles.", cfg.DirectoryInterval.Seconds()),
		gauge("mcs_metrics_directory_timeout_seconds", "Cooperative time budget for each directory scan.", cfg.DirectoryTimeout.Seconds()),
		gauge("mcs_metrics_directories_configured", "Number of configured directory roots.", float64(len(cfg.Directories))),
	}
	now := time.Now()
	for name, sample := range samples {
		points = append(points,
			gauge("mcs_directory_scan_success", "Whether the last completed scan succeeded; inspect last-success age before using retained sizes.", boolean(sample.Success), "directory", name),
			gauge("mcs_directory_scan_in_progress", "Whether this directory is currently being scanned.", boolean(sample.InFlight), "directory", name),
			counter("mcs_directory_scan_errors_total", "Failed complete directory scans, excluding reload and shutdown cancellations.", float64(sample.Errors), "directory", name))
		if !sample.LastAttempt.IsZero() {
			points = append(points,
				gauge("mcs_directory_scan_last_attempt_timestamp_seconds", "Start time of the most recent directory scan attempt.", unix(sample.LastAttempt), "directory", name),
				gauge("mcs_directory_scan_duration_seconds", "Duration of the most recent completed scan attempt.", sample.Duration.Seconds(), "directory", name))
		}
		if !sample.LastSuccess.IsZero() {
			points = append(points,
				gauge("mcs_directory_scan_last_success_timestamp_seconds", "Completion time of the most recent complete successful directory scan.", unix(sample.LastSuccess), "directory", name),
				gauge("mcs_directory_scan_age_seconds", "Age of the last successful complete directory totals.", math.Max(0, now.Sub(sample.LastSuccess).Seconds()), "directory", name),
				gauge("mcs_directory_size_bytes", "Logical regular-file bytes from the last successful scan; hard links counted once per root.", float64(sample.Totals.Bytes), "directory", name),
				gauge("mcs_directory_allocated_bytes", "Allocated regular-file blocks times 512 from the last successful scan; excludes directory metadata.", float64(sample.Totals.Allocated), "directory", name),
				gauge("mcs_directory_files", "Unique regular files from the last successful scan.", float64(sample.Totals.Files), "directory", name),
				gauge("mcs_directory_directories", "Unique scanned directories, including the configured root.", float64(sample.Totals.Directories), "directory", name),
				gauge("mcs_directory_symlinks_skipped", "Symlinks excluded from the last successful scan.", float64(sample.Totals.Symlinks), "directory", name),
				gauge("mcs_directory_special_files_skipped", "Nonregular nondirectory files excluded from the last successful scan.", float64(sample.Totals.Special), "directory", name),
				gauge("mcs_directory_mounts_skipped", "Directories on another device excluded from the last successful scan.", float64(sample.Totals.Mounts), "directory", name))
		}
	}
	return points
}
