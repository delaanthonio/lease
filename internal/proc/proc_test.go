package proc

import (
	"math"
	"os"
	"os/exec"
	"testing"
)

func TestAliveAndFingerprintForSelf(t *testing.T) {
	pid := os.Getpid()
	if !Alive(pid) {
		t.Fatal("own pid reported dead")
	}
	fp := Fingerprint(pid)
	if fp == "" {
		t.Fatal("own pid has no fingerprint")
	}
	if HolderIsStale(pid, fp) != nil {
		t.Fatal("own pid with own fingerprint reported stale")
	}
	// A mismatched fingerprint means the pid was reused by another process.
	if err := HolderIsStale(pid, fp+"-different"); err == nil {
		t.Fatal("mismatched fingerprint must report the pid as reused")
	}
}

func TestStaleForDeadPid(t *testing.T) {
	if err := HolderIsStale(999999999, ""); err == nil {
		t.Fatal("an impossible pid must be stale")
	}
	if err := HolderIsStale(0, ""); err == nil {
		t.Fatal("pid 0 must be stale")
	}
	if err := HolderIsStale(-1, ""); err == nil {
		t.Fatal("a negative pid must be stale")
	}
}

func TestPidsOutsidePidTRangeAreInvalid(t *testing.T) {
	// pid_t is a signed 32-bit type: values above its maximum can never
	// name a process, and a truncated or wrapped kill call with one could
	// signal an unintended target. Zero and negatives carry signal
	// semantics (process group, all permitted processes) and are equally
	// invalid as holder records.
	for _, pid := range []int{0, -1, math.MinInt32, math.MaxInt32 + 1, math.MaxInt64} {
		if Alive(pid) {
			t.Fatalf("Alive(%d) = true, want false", pid)
		}
		if Fingerprint(pid) != "" {
			t.Fatalf("Fingerprint(%d) = %q, want empty", pid, Fingerprint(pid))
		}
		if err := HolderIsStale(pid, ""); err == nil {
			t.Fatalf("HolderIsStale(%d) = nil, want invalid-pid error", pid)
		}
	}
}

func TestFingerprintChangesForNewProcess(t *testing.T) {
	// A short-lived child gets one fingerprint; a second child on the same
	// pid slot is not guaranteed, so instead assert a finished process has
	// no fingerprint at all.
	cmd := exec.Command("/bin/sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if Fingerprint(pid) == "" {
		t.Fatal("running child has no fingerprint")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()
	// Once reaped, the pid is gone: no fingerprint, and stale for holders.
	if Alive(pid) {
		t.Skip("pid was reused too quickly to assert death")
	}
	if Fingerprint(pid) != "" {
		t.Fatal("dead pid still has a fingerprint")
	}
	if err := HolderIsStale(pid, "anything"); err == nil {
		t.Fatal("a reaped pid must count as stale")
	}
}
