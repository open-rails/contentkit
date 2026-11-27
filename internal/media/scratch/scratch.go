// Package scratch gives a media worker process a scratch directory of its
// own under a root other processes may share. A lock on it lasts as long as
// the process: Open removes the directories of processes that died with work
// on disk, and never one whose process still runs.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// base is the folder under the root that holds each process's directory
	// (<id>/) beside its lock (<id>.lock).
	base = "contentkit-media-worker"
	// guard serializes Opens under one root: a sweep never meets a
	// directory being made.
	guard = ".open"
)

// Dir is one process's scratch directory, locked until Close or exit.
type Dir struct {
	path string
	lock *os.File
}

// Open removes the directories left by dead processes under root (default
// os.TempDir()), then makes and locks this process's own.
func Open(root string) (*Dir, error) {
	if root == "" {
		root = os.TempDir()
	}
	parent := filepath.Join(root, base)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	g, err := os.OpenFile(filepath.Join(parent, guard), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	defer g.Close()
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	if err := sweep(parent); err != nil {
		return nil, fmt.Errorf("scratch: sweep %s: %w", parent, err)
	}
	lock, err := os.CreateTemp(parent, "*.lock")
	if err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	d := &Dir{path: strings.TrimSuffix(lock.Name(), ".lock"), lock: lock}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("scratch: %w", err), d.Close())
	}
	if err := os.Mkdir(d.path, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("scratch: %w", err), d.Close())
	}
	return d, nil
}

// Path is the directory jobs write their scratch in.
func (d *Dir) Path() string { return d.path }

// Close removes the directory, then its lock.
func (d *Dir) Close() error {
	return errors.Join(os.RemoveAll(d.path), os.Remove(d.lock.Name()), d.lock.Close())
}

// sweep removes each directory whose lock no process holds, and any
// directory without a lock.
func sweep(parent string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := filepath.Join(parent, e.Name())
		switch dir, ok := strings.CutSuffix(name, ".lock"); {
		case e.Name() == guard:
		case ok:
			errs = append(errs, reclaim(dir, name))
		default:
			if _, err := os.Lstat(name + ".lock"); errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, os.RemoveAll(name))
			}
		}
	}
	return errors.Join(errs...)
}

// reclaim removes dir and its lock when no process holds the lock.
func reclaim(dir, lock string) error {
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // its process closed it meanwhile
	} else if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
		return nil // its process runs
	} else if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// A lock already gone: its process closed it after we opened it.
	if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
