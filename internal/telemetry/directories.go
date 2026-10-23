package telemetry

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
			if err := w.visit(dir, entry.Name(), depth); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (w *directoryWalk) visit(dir *os.File, name string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.visited++
	if w.visited > maxDirectoryEntries {
		return errors.New("directory entry count exceeds safety limit")
	}
	// With O_PATH|O_NOFOLLOW a symlink is opened as the link itself. Other
	// nonregular files are also inspected without activating a device.
	fd, err := syscall.Openat(int(dir.Fd()), name, directoryPathOnly|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
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
		return w.countFile(child)
	case syscall.S_IFDIR:
		return w.descend(dir, name, child, depth)
	default:
		w.totals.Special++
	}
	return nil
}

func (w *directoryWalk) countFile(file syscall.Stat_t) error {
	id := fileIdentity{uint64(file.Dev), file.Ino}
	if w.seen[id] {
		return nil
	}
	w.seen[id] = true
	if file.Size < 0 || file.Blocks < 0 {
		return errors.New("invalid file size")
	}
	bytes, blocks := uint64(file.Size), uint64(file.Blocks)
	if blocks > math.MaxUint64/512 || math.MaxUint64-w.totals.Bytes < bytes || math.MaxUint64-w.totals.Allocated < blocks*512 {
		return errors.New("directory size overflow")
	}
	w.totals.Files++
	w.totals.Bytes += bytes
	w.totals.Allocated += blocks * 512
	return nil
}

func (w *directoryWalk) descend(dir *os.File, name string, expected syscall.Stat_t, depth int) error {
	if uint64(expected.Dev) != w.device {
		w.totals.Mounts++
		return nil
	}
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	opened := os.NewFile(uintptr(fd), name)
	defer opened.Close()
	var check syscall.Stat_t
	if err := syscall.Fstat(fd, &check); err != nil {
		return err
	}
	if expected.Dev != check.Dev || expected.Ino != check.Ino {
		return errors.New("directory changed during scan")
	}
	return w.walk(opened, check, depth+1)
}
