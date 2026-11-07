// Package lock manages the kernel file locks behind lease. Each slot of a
// resource is one file; holding the slot means holding an exclusive flock
// on its file. The kernel releases the lock when the holding process dies,
// which is what makes lease robust against crashes and SIGKILLs without a
// daemon.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Dir returns the lock-file directory for a resource under the state root.
func Dir(root, resource string) string {
	return filepath.Join(root, "locks", resource)
}

// OpenSlot opens (creating if needed) the lock file for one slot of a
// resource. The caller is responsible for closing the file.
func OpenSlot(root, resource string, slot int) (*os.File, error) {
	dir := Dir(root, resource)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("slot-%d.lock", slot))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// TryLock takes an exclusive flock without blocking. It reports false when
// another process currently holds the lock.
func TryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK {
		return false, nil
	}
	return false, fmt.Errorf("lock %s: %w", f.Name(), err)
}

// Unlock releases an exclusive flock previously taken on the file.
func Unlock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err == nil {
		return nil
	}
	return fmt.Errorf("unlock %s: %w", f.Name(), err)
}

// Probe reports whether a slot is currently locked by some process. The
// probe takes and immediately releases the lock, so it never changes who
// holds the slot; it only observes it.
func Probe(root, resource string, slot int) (used bool, err error) {
	f, err := OpenSlot(root, resource, slot)
	if err != nil {
		return false, err
	}
	defer f.Close()
	ok, err := TryLock(f)
	if err != nil {
		return false, err
	}
	if ok {
		if err := Unlock(f); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}
