package proc

import (
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
