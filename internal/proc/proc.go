// Package proc answers two questions about other processes on this
// machine: is this PID still alive, and is it still the same process
// incarnation a lease holder record was written for. Together they detect
// stale holders whose PID was recycled after a crash or a SIGKILL.
package proc

import (
	"errors"
	"fmt"
	"math"
	"syscall"
)

// validPID reports whether a value can name a real process: positive and
// within the kernel's pid_t range. Values outside it can never name a
// process, but they carry signal semantics (0 targets the whole process
// group, -1 every permitted process), so a corrupt or group-written
// holder record must never reach kill with one.
func validPID(pid int) bool {
	return pid > 0 && pid <= math.MaxInt32
}

// Alive reports whether a process with this PID currently exists. A PID
// that exists but is owned by another user still counts as alive.
func Alive(pid int) bool {
	if !validPID(pid) {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Fingerprint returns a value that changes when the process behind a PID
// dies and the PID is later reused. The empty string means the PID does
// not exist or its identity could not be read.
func Fingerprint(pid int) string {
	if !Alive(pid) {
		return ""
	}
	return fingerprint(pid)
}

// HolderIsStale reports whether the process recorded in a holder record is
// gone or was replaced by a different process reusing the PID. An unknown
// fingerprint counts as stale so a record that cannot be verified never
// blocks a slot forever.
func HolderIsStale(pid int, recorded string) error {
	if !validPID(pid) {
		return fmt.Errorf("holder pid %d is not valid", pid)
	}
	if !Alive(pid) {
		return fmt.Errorf("holder pid %d is no longer running", pid)
	}
	if recorded != "" && Fingerprint(pid) != recorded {
		return fmt.Errorf("holder pid %d was reused by a different process", pid)
	}
	return nil
}
