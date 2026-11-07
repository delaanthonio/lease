package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/delaanthonio/lease/internal/lock"
	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/store"
)

// The integration suite builds the real lease binary and drives it as
// external processes, the way workers, skills, and CI jobs would.

var leaseBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lease-integration-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	// Build from the module root, two levels above this package.
	root, err := filepath.Abs(filepath.Join(".", "..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "lease")
	build := exec.Command("go", "build", "-o", bin, "./cmd/lease")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build lease: %v\n", err)
		os.Exit(1)
	}
	leaseBin = bin
	os.Exit(m.Run())
}

// leaseEnv isolates one test's state directory.
func leaseEnv(t *testing.T) (dir string, env []string) {
	t.Helper()
	dir = t.TempDir()
	return dir, append(os.Environ(), "LEASE_STATE_DIR="+dir)
}

// runLease runs one lease invocation and returns stdout, stderr, exit code.
func runLease(t *testing.T, env []string, timeout time.Duration, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(leaseBin, args...)
	cmd.Env = env
	var out, errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			code = 1
			var exitErr *exec.ExitError
			if asExitError(err, &exitErr) {
				code = exitErr.ExitCode()
			}
		}
		return out.String(), errBuf.String(), code
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("lease %v timed out after %s", args, timeout)
		return "", "", -1
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestRunContentionTwoCallers(t *testing.T) {
	_, env := leaseEnv(t)

	// First caller holds the resource for a while.
	holder := exec.Command(leaseBin, "run", "ios-simulator", "--", "/bin/sleep", "6")
	holder.Env = env
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	waitForHolder(t, env, "ios-simulator")

	// A second caller with --no-wait fails with the busy exit code and a
	// message naming the holder.
	_, errOut, code := runLease(t, env, 10*time.Second, "run", "ios-simulator", "--no-wait", "--", "/bin/echo", "nope")
	if code != 75 {
		t.Fatalf("busy caller exit = %d, want 75; stderr: %s", code, errOut)
	}
	if !strings.Contains(errOut, "ios-simulator") {
		t.Fatalf("busy message does not name the resource: %s", errOut)
	}

	// A waiting caller gets the lock once the holder exits on its own.
	start := time.Now()
	stdout, _, code := runLease(t, env, 15*time.Second, "run", "ios-simulator", "--wait", "10s", "--", "/bin/echo", "got-it")
	if code != 0 || !strings.Contains(stdout, "got-it") {
		t.Fatalf("waiter exit=%d stdout=%q", code, stdout)
	}
	if elapsed := time.Since(start); elapsed > 9*time.Second {
		t.Fatalf("waiter waited %s; the holder should have exited after 6s", elapsed)
	}
}

func TestHolderKilledIsTakenOver(t *testing.T) {
	dir, env := leaseEnv(t)
	ready := filepath.Join(dir, "child-ready")
	holder := exec.Command(leaseBin, "run", "db", "--", "/bin/sh", "-c", `touch "$READY"; exec /bin/sleep 3`)
	holder.Env = append(env, "READY="+ready)
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	childReady := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			childReady = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !childReady {
		t.Fatal("child did not start")
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	stdout, _, code := runLease(t, env, 5*time.Second, "status", "--json", "db")
	if code != 0 || !strings.Contains(stdout, `"used":1`) || strings.Contains(stdout, `"stale":true`) {
		t.Fatalf("surviving child not shown held: %s", stdout)
	}
	_, _, code = runLease(t, env, 5*time.Second, "run", "db", "--no-wait", "--", "/bin/echo", "bad")
	if code != 75 {
		t.Fatalf("surviving child failed to retain lock: %d", code)
	}
	stdout, errOut, code := runLease(t, env, 8*time.Second, "run", "db", "--", "/bin/echo", "took-over")
	if code != 0 || !strings.Contains(stdout, "took-over") {
		t.Fatalf("takeover exit=%d stdout=%q stderr=%q", code, stdout, errOut)
	}
}

func TestSlotsCountCallers(t *testing.T) {
	dir, env := leaseEnv(t)
	cfg := filepath.Join(dir, "config.toml")
	writeFile(t, cfg, "[resources.ports]\nslots = 2\n")

	var wg sync.WaitGroup
	codes := make(chan int, 3)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, code := runLease(t, env, 15*time.Second, "run", "--config", cfg, "ports", "--wait", "10s", "--", "/bin/sleep", "2")
			codes <- code
		}()
	}
	// The third caller finds both slots taken; wait until the two holders
	// are established so this is a real slot exhaustion.
	waitForUsed(t, env, cfg, "ports", 2)
	_, _, code := runLease(t, env, 15*time.Second, "run", "--config", cfg, "ports", "--no-wait", "--", "/bin/echo", "nope")
	if code != 75 {
		t.Fatalf("third caller exit = %d, want 75", code)
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 0 {
			t.Fatalf("one of two slot holders exited %d, want 0", c)
		}
	}
}

func TestMultiLockAcquisitionOrdering(t *testing.T) {
	_, env := leaseEnv(t)

	// Two callers each want two resources; both must succeed without
	// deadlocking, in the fixed sorted order.
	holder := exec.Command(leaseBin, "run", "zeta", "alpha", "--", "/bin/sleep", "2")
	holder.Env = env
	if err := holder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForHolder(t, env, "alpha")
	waitForHolder(t, env, "zeta")

	stdout, _, code := runLease(t, env, 15*time.Second, "run", "alpha", "zeta", "--wait", "10s", "--", "/bin/echo", "both-mine")
	if code != 0 || !strings.Contains(stdout, "both-mine") {
		t.Fatalf("second caller exit=%d stdout=%q (deadlock or lost race)", code, stdout)
	}
	_ = holder.Wait()
}

func TestStatusJSONReportsHolderAndWaiting(t *testing.T) {
	dir, env := leaseEnv(t)
	cfg := filepath.Join(dir, "config.toml")
	writeFile(t, cfg, "[resources.ports]\nslots = 2\n")

	holder := exec.Command(leaseBin, "run", "--config", cfg, "ports", "--", "/bin/sleep", "8")
	holder.Env = env
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	waitForHolder(t, env, "ports")

	// A caller wanting both slots queues a waiter ticket.
	waiter := exec.Command(leaseBin, "run", "--config", cfg, "ports", "--slots", "2", "--", "/bin/sleep", "8")
	waiter.Env = env
	if err := waiter.Start(); err != nil {
		t.Fatalf("start waiter: %v", err)
	}
	defer func() {
		_ = waiter.Process.Kill()
		_ = waiter.Wait()
	}()
	time.Sleep(300 * time.Millisecond)

	stdout, _, code := runLease(t, env, 10*time.Second, "status", "--config", cfg, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	var views []struct {
		Resource string `json:"resource"`
		Total    int    `json:"total"`
		Used     int    `json:"used"`
		Waiting  int    `json:"waiting"`
		Holders  []struct {
			Slot    int    `json:"slot"`
			Holder  string `json:"holder"`
			Command string `json:"command"`
		} `json:"holders"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &views); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, stdout)
	}
	if len(views) != 1 || views[0].Resource != "ports" {
		t.Fatalf("unexpected views: %+v", views)
	}
	if views[0].Total != 2 || views[0].Used != 1 {
		t.Fatalf("slots = %d/%d, want 1/2", views[0].Used, views[0].Total)
	}
	if len(views[0].Holders) != 1 || views[0].Holders[0].Command != "sleep" {
		t.Fatalf("holder not reported: %+v", views[0].Holders)
	}
	if views[0].Waiting != 1 {
		t.Fatalf("waiting = %d, want 1", views[0].Waiting)
	}
}

func TestAcquireReleaseTokenFlow(t *testing.T) {
	_, env := leaseEnv(t)

	stdout, _, code := runLease(t, env, 10*time.Second, "acquire", "db")
	if code != 0 {
		t.Fatalf("acquire exit = %d", code)
	}
	token := strings.TrimSpace(stdout)
	if !strings.HasPrefix(token, "l-") {
		t.Fatalf("acquire printed %q, want an l- token", token)
	}

	// The slot shows as held while the token is unreleased.
	stdout, _, _ = runLease(t, env, 10*time.Second, "status", "--json")
	if !strings.Contains(stdout, `"used":1`) {
		t.Fatalf("status after acquire shows the slot free: %s", stdout)
	}

	// A second acquire of the single-slot resource is busy.
	_, _, code = runLease(t, env, 10*time.Second, "acquire", "db", "--no-wait")
	if code != 75 {
		t.Fatalf("second acquire exit = %d, want 75", code)
	}

	// Releasing frees the slot.
	_, _, code = runLease(t, env, 10*time.Second, "release", token)
	if code != 0 {
		t.Fatalf("release exit = %d", code)
	}
	stdout, _, code = runLease(t, env, 10*time.Second, "status", "--json")
	if code != 0 || strings.Contains(stdout, `"used":1`) {
		t.Fatalf("status after release still shows the slot held: %s", stdout)
	}
}

func TestForceReleaseLogsReason(t *testing.T) {
	dir, env := leaseEnv(t)

	// A stale record: the recorded process is long gone.
	// Acquire and kill the keeper, leaving the record behind.
	stdout, _, code := runLease(t, env, 10*time.Second, "acquire", "db")
	if code != 0 {
		t.Fatalf("acquire exit = %d", code)
	}
	keeper := keeperPIDFor(t, env, "db")
	if err := syscall.Kill(keeper, syscall.SIGKILL); err != nil {
		t.Fatalf("kill keeper: %v", err)
	}

	stdout, _, code = runLease(t, env, 10*time.Second, "force-release", "db", "--reason", "stuck after crash")
	if code != 0 || !strings.Contains(stdout, "cleared") {
		t.Fatalf("force-release exit=%d stdout=%q", code, stdout)
	}

	// The reason and previous holder are in the history.
	stdout, _, code = runLease(t, env, 10*time.Second, "status", "--json")
	_ = stdout
	events := readFile(t, filepath.Join(dir, "events.ndjson"))
	if !strings.Contains(events, "force-release") || !strings.Contains(events, "stuck after crash") {
		t.Fatalf("force-release reason not logged: %s", events)
	}
}

// waitForUsed polls status until the resource shows this many used slots.
func waitForUsed(t *testing.T, env []string, cfg, resource string, used int) {
	t.Helper()
	want := fmt.Sprintf(`"used":%d`, used)
	for i := 0; i < 100; i++ {
		stdout, _, _ := runLease(t, env, 10*time.Second, "status", "--config", cfg, "--json", resource)
		if strings.Contains(stdout, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("resource %q never showed %d used slots", resource, used)
}

// waitForHolder polls status until the resource shows a holder.
func waitForHolder(t *testing.T, env []string, resource string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		stdout, _, _ := runLease(t, env, 10*time.Second, "status", "--json", resource)
		if strings.Contains(stdout, `"used":1`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("resource %q never showed as held", resource)
}

// keeperPIDFor reads the pid of the keeper holding a resource from the
// status JSON.
func keeperPIDFor(t *testing.T, env []string, resource string) int {
	t.Helper()
	stdout, _, _ := runLease(t, env, 10*time.Second, "status", "--json", resource)
	var views []struct {
		Holders []struct {
			PID int `json:"pid"`
		} `json:"holders"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &views); err != nil || len(views) == 0 || len(views[0].Holders) == 0 {
		t.Fatalf("cannot read keeper pid from %s: %v", stdout, err)
	}
	return views[0].Holders[0].PID
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestManualOwnerDeathReleasesLease(t *testing.T) {
	dir, env := leaseEnv(t)
	ready := filepath.Join(dir, "ready")
	script := exec.Command("/bin/sh", "-c", `"$LEASE_BIN" acquire db >/dev/null || exit; touch "$READY"; exec sleep 30`)
	script.Env = append(env, "LEASE_BIN="+leaseBin, "READY="+ready)
	if err := script.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = script.Process.Kill(); _ = script.Wait() }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	waitForHolder(t, env, "db")
	if err := script.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = script.Wait()
	_, errOut, code := runLease(t, env, 5*time.Second, "run", "db", "--wait", "3s", "--", "/bin/echo")
	if code != 0 {
		t.Fatalf("owner death did not release lease: %d %s", code, errOut)
	}
}

func TestKilledWaiterIsRemoved(t *testing.T) {
	_, env := leaseEnv(t)
	holder := exec.Command(leaseBin, "run", "db", "--", "/bin/sleep", "4")
	holder.Env = env
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	waitForHolder(t, env, "db")
	waiter := exec.Command(leaseBin, "run", "db", "--", "/bin/echo")
	waiter.Env = env
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	observed := false
	for i := 0; i < 100; i++ {
		out, _, _ := runLease(t, env, 5*time.Second, "status", "--json", "db")
		if strings.Contains(out, `"waiting":1`) {
			observed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !observed {
		_ = waiter.Process.Kill()
		_ = waiter.Wait()
		t.Fatal("waiter never appeared")
	}
	_ = waiter.Process.Kill()
	_ = waiter.Wait()
	out, _, code := runLease(t, env, 5*time.Second, "status", "--json", "db")
	if code != 0 || !strings.Contains(out, `"waiting":0`) {
		t.Fatalf("stale waiter remains: %s", out)
	}
}

func TestSharedConfigAndManualSet(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	state := filepath.Join(dir, "shared")
	writeFile(t, cfg, fmt.Sprintf("dir = %q\nshared = true\n[resources.pool]\nslots = 2\n", state))
	env := append(os.Environ(), "LEASE_CONFIG="+cfg, "LEASE_DIR=", "LEASE_STATE_DIR=")
	token, stderr, code := runLease(t, env, 5*time.Second, "acquire", "pool", "--slots", "2")
	if code != 0 {
		t.Fatalf("acquire: %d %s", code, stderr)
	}
	defer runLease(t, env, 5*time.Second, "release", strings.TrimSpace(token))
	_, _, code = runLease(t, env, 5*time.Second, "run", "pool", "--no-wait", "--", "/bin/echo")
	if code != 75 {
		t.Fatalf("configured shared state did not coordinate: %d", code)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{state, 0770}, {filepath.Join(state, "locks", paths.ResourceComponent("pool"), "slot-1.lock"), 0660}, {filepath.Join(state, "holders", paths.ResourceComponent("pool"), "1.json"), 0660}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("mode %s: %v %v", item.path, info, err)
		}
	}
}

func TestDescendantRetainsLockAfterWrapperExit(t *testing.T) {
	_, env := leaseEnv(t)
	_, stderr, code := runLease(t, env, 5*time.Second, "run", "db", "--", "/bin/sh", "-c", "sleep 2 </dev/null >/dev/null 2>&1 &")
	if code != 0 {
		t.Fatalf("wrapper failed: %d %s", code, stderr)
	}
	out, _, code := runLease(t, env, 5*time.Second, "status", "--json", "db")
	if code != 0 || !strings.Contains(out, `"used":1`) {
		t.Fatalf("descendant not shown as held: %s", out)
	}
	_, _, code = runLease(t, env, 5*time.Second, "run", "db", "--no-wait", "--", "/bin/echo")
	if code != 75 {
		t.Fatalf("descendant lost its lock: %d", code)
	}
	_, stderr, code = runLease(t, env, 5*time.Second, "run", "db", "--wait", "4s", "--", "/bin/echo")
	if code != 0 {
		t.Fatalf("lock not released after descendant: %d %s", code, stderr)
	}
}

func TestPartialAcquisitionVisible(t *testing.T) {
	_, env := leaseEnv(t)
	blocker := exec.Command(leaseBin, "run", "b", "--", "/bin/sleep", "4")
	blocker.Env = env
	if err := blocker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Process.Kill(); _ = blocker.Wait() }()
	waitForHolder(t, env, "b")
	waiter := exec.Command(leaseBin, "run", "a", "b", "--", "/bin/echo")
	waiter.Env = append(env, "FM_TASK_ID=partial")
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiter.Process.Kill(); _ = waiter.Wait() }()
	for i := 0; i < 100; i++ {
		out, _, code := runLease(t, env, 5*time.Second, "status", "--json", "a")
		if code == 0 && strings.Contains(out, "task:partial") {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("partial holder never became visible")
}

func TestManualReleaseCompletesSet(t *testing.T) {
	_, env := leaseEnv(t)
	token, stderr, code := runLease(t, env, 5*time.Second, "acquire", "a", "b")
	if code != 0 {
		t.Fatalf("acquire: %s", stderr)
	}
	_, stderr, code = runLease(t, env, 5*time.Second, "release", strings.TrimSpace(token))
	if code != 0 {
		t.Fatalf("release: %s", stderr)
	}
	for _, name := range []string{"a", "b"} {
		_, stderr, code = runLease(t, env, 5*time.Second, "run", name, "--no-wait", "--", "/bin/echo")
		if code != 0 {
			t.Fatalf("%s still held: %s", name, stderr)
		}
	}
}

func TestSharedHistoryRecordsBothParticipants(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	state := filepath.Join(dir, "shared")
	writeFile(t, cfg, fmt.Sprintf("dir = %q\nshared = true\n", state))
	env := append(os.Environ(), "LEASE_CONFIG="+cfg, "LEASE_DIR=", "LEASE_STATE_DIR=")
	for _, user := range []string{"first", "second"} {
		_, stderr, code := runLease(t, append(env, "FM_TASK_ID="+user), 5*time.Second, "run", "db", "--", "/bin/echo", "secret")
		if code != 0 {
			t.Fatalf("participant %s: %s", user, stderr)
		}
	}
	raw, err := os.ReadFile(filepath.Join(state, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(state, "events.ndjson"))
	if err != nil || info.Mode().Perm() != 0660 {
		t.Fatalf("history mode: %v %v", info, err)
	}
	if !strings.Contains(string(raw), "task:first") || !strings.Contains(string(raw), "task:second") || strings.Contains(string(raw), "secret") {
		t.Fatalf("shared history: %s", raw)
	}
	_, _, code := runLease(t, env, 5*time.Second, "run", "db", "--record-command", "--", "/bin/echo", "secret")
	if code == 0 {
		t.Fatal("shared mode accepted full command recording")
	}
}

func TestInvalidAcquisitionFlagsAreUsageErrors(t *testing.T) {
	_, env := leaseEnv(t)
	for _, command := range []string{"run", "acquire"} {
		for _, flags := range [][]string{{"--slots", "0"}, {"--slots", "-2"}, {"--wait", "-1s"}} {
			args := append([]string{command, "db"}, flags...)
			if command == "run" {
				args = append(args, "--", "/bin/echo", "unexpected")
			}
			out, stderr, code := runLease(t, env, 5*time.Second, args...)
			if code != 2 || strings.Contains(out, "unexpected") {
				t.Fatalf("%v: exit %d, output %q, stderr %q", args, code, out, stderr)
			}
		}
	}
}

func TestHistoryLockHonorsDeadlineAndCancellation(t *testing.T) {
	dir, env := leaseEnv(t)
	history, err := os.OpenFile(filepath.Join(dir, "events.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if err := syscall.Flock(int(history.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(history.Fd()), syscall.LOCK_UN)
	slot, err := lock.OpenSlot(dir, "db", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Close()
	ok, err := lock.TryLock(slot)
	if err != nil || !ok {
		t.Fatalf("lock resource: %v", err)
	}
	defer lock.Unlock(slot)
	for _, command := range []string{"run", "acquire"} {
		args := []string{command, "db", "--wait", "100ms"}
		if command == "run" {
			args = append(args, "--", "/bin/echo", "unexpected")
		}
		start := time.Now()
		out, stderr, code := runLease(t, env, 3*time.Second, args...)
		if code != 75 || time.Since(start) > 2*time.Second || strings.Contains(out, "unexpected") {
			t.Fatalf("%v: exit %d, output %q, stderr %q", args, code, out, stderr)
		}
	}
	cmd := exec.Command(leaseBin, "run", "a", "db", "--", "/bin/echo", "unexpected")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	waitForHolder(t, env, "a")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not interrupt acquisition")
	}
	used, err := lock.Probe(dir, "a", 1)
	if err != nil || used {
		t.Fatalf("partial lock retained: %v %v", used, err)
	}
}

func TestNoWaitDoesNotWaitForHistoryLock(t *testing.T) {
	for _, command := range []string{"run", "acquire"} {
		for _, scenario := range []string{"free", "busy", "takeover"} {
			for _, wait := range []string{"--no-wait", "--wait=500ms", "--wait=0"} {
				if scenario == "busy" && wait == "--wait=0" {
					continue
				}
				t.Run(command+"/"+scenario+"/"+wait, func(t *testing.T) {
					dir, env := leaseEnv(t)
					if scenario == "busy" {
						slot, err := lock.OpenSlot(dir, "db", 1)
						if err != nil {
							t.Fatal(err)
						}
						defer slot.Close()
						ok, err := lock.TryLock(slot)
						if err != nil || !ok {
							t.Fatalf("lock resource: %v", err)
						}
						defer lock.Unlock(slot)
					}
					if scenario == "takeover" {
						if err := store.NewFileStore(dir).SetHolder(&store.Holder{Resource: "db", Slot: 1, PID: 999999999, ProcStart: "gone"}); err != nil {
							t.Fatal(err)
						}
					}
					history, err := os.OpenFile(filepath.Join(dir, "events.lock"), os.O_CREATE|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					defer history.Close()
					if err := syscall.Flock(int(history.Fd()), syscall.LOCK_EX); err != nil {
						t.Fatal(err)
					}
					defer syscall.Flock(int(history.Fd()), syscall.LOCK_UN)
					args := []string{command, "db", wait}
					if command == "run" {
						args = append(args, "--", "/bin/echo", "executed")
					}
					start := time.Now()
					out, stderr, code := runLease(t, env, 3*time.Second, args...)
					if time.Since(start) > 2*time.Second {
						t.Fatalf("history contention delayed %v", args)
					}
					if scenario == "busy" {
						if code != 75 {
							t.Fatalf("busy resource exit %d: %s", code, stderr)
						}
						return
					}
					if code != 0 {
						t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out, stderr)
					}
					if command == "run" && !strings.Contains(out, "executed") {
						t.Fatalf("command did not run: %q", out)
					}
					if command == "acquire" {
						token := strings.TrimSpace(out)
						if !strings.HasPrefix(token, "l-") {
							t.Fatalf("missing token: %q", out)
						}
						_, stderr, code = runLease(t, env, 3*time.Second, "release", token)
						if code != 0 {
							t.Fatalf("release: %s", stderr)
						}
					}
					used, err := lock.Probe(dir, "db", 1)
					if err != nil || used {
						t.Fatalf("resource not released: %v %v", used, err)
					}
				})
			}
		}
	}
}

func TestNoWaitReleaseDoesNotWaitForHistoryLock(t *testing.T) {
	dir, env := leaseEnv(t)
	ready := filepath.Join(dir, "child-ready")
	finish := filepath.Join(dir, "finish")
	cmd := exec.Command(leaseBin, "run", "db", "--no-wait", "--", "/bin/sh", "-c", `touch "$READY"; while [ ! -f "$FINISH" ]; do sleep 0.05; done`)
	cmd.Env = append(env, "READY="+ready, "FINISH="+finish)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	readySeen := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			readySeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !readySeen {
		t.Fatal("child did not start")
	}
	history, err := os.OpenFile(filepath.Join(dir, "events.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if err := syscall.Flock(int(history.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(history.Fd()), syscall.LOCK_UN)
	if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release waited for history")
	}
	used, err := lock.Probe(dir, "db", 1)
	if err != nil || used {
		t.Fatalf("resource still held: %v %v", used, err)
	}
}

func TestNoWaitWithFreeResourceAndHistory(t *testing.T) {
	_, env := leaseEnv(t)
	out, stderr, code := runLease(t, env, 3*time.Second, "run", "db", "--no-wait", "--", "/bin/echo", "ok")
	if code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("run: exit %d stdout %q stderr %q", code, out, stderr)
	}
	token, stderr, code := runLease(t, env, 3*time.Second, "acquire", "db", "--no-wait")
	if code != 0 {
		t.Fatalf("acquire: exit %d stderr %q", code, stderr)
	}
	_, stderr, code = runLease(t, env, 3*time.Second, "release", strings.TrimSpace(token))
	if code != 0 {
		t.Fatalf("release: exit %d stderr %q", code, stderr)
	}
}

func TestManualBusyReportsResourceAndHolder(t *testing.T) {
	_, env := leaseEnv(t)
	holder := exec.Command(leaseBin, "run", "db", "--", "/bin/sleep", "5")
	holder.Env = append(env, "FM_TASK_ID=busy-holder")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	found := false
	for i := 0; i < 100; i++ {
		out, _, _ := runLease(t, env, 3*time.Second, "status", "--json", "db")
		if strings.Contains(out, "task:busy-holder") {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatal("holder was not recorded")
	}
	for _, names := range [][]string{{"db"}, {"a", "db"}} {
		args := append([]string{"acquire"}, names...)
		args = append(args, "--no-wait")
		_, stderr, code := runLease(t, env, 3*time.Second, args...)
		if code != 75 || !strings.Contains(stderr, `resource "db"`) || !strings.Contains(stderr, "task:busy-holder") {
			t.Fatalf("%v: exit %d stderr %q", args, code, stderr)
		}
	}
}

func TestStaleManualReleaseSkipsBusyHistory(t *testing.T) {
	dir, env := leaseEnv(t)
	st := store.NewFileStore(dir)
	if err := st.SetHolder(&store.Holder{Resource: "db", Slot: 1, Token: "l-stale", PID: 999999999, ProcStart: "gone"}); err != nil {
		t.Fatal(err)
	}
	history, err := os.OpenFile(filepath.Join(dir, "events.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if err := syscall.Flock(int(history.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(history.Fd()), syscall.LOCK_UN)
	_, stderr, code := runLease(t, env, 3*time.Second, "release", "l-stale")
	if code != 0 {
		t.Fatalf("release: exit %d stderr %q", code, stderr)
	}
	h, err := st.Holder("db", 1)
	if err != nil || h != nil {
		t.Fatalf("stale holder survived: %+v %v", h, err)
	}
}

func TestForceReleaseKeepsHolderWhenHistoryLocked(t *testing.T) {
	dir, env := leaseEnv(t)
	st := store.NewFileStore(dir)
	h := &store.Holder{Resource: "db", Slot: 1, PID: 99999999}
	if err := st.SetHolder(h); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_, stderr, code := runLease(t, env, 5*time.Second, "force-release", "db", "--reason", "repair")
	if code == 0 || !strings.Contains(stderr, "record force-release reason") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	got, err := st.Holder("db", 1)
	if err != nil || got == nil || *got != *h {
		t.Fatalf("holder changed: %+v %v", got, err)
	}
}

func TestNamedStatusPrunesExpiredHistory(t *testing.T) {
	dir, env := leaseEnv(t)
	raw, _ := json.Marshal(store.Event{At: time.Now().Add(-31 * 24 * time.Hour), Kind: store.EventAcquire, Detail: "expired-secret"})
	if err := os.WriteFile(filepath.Join(dir, "events.ndjson"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runLease(t, env, 5*time.Second, "status", "db")
	if code != 0 {
		t.Fatalf("status exit=%d: %s", code, stderr)
	}
	if data := readFile(t, filepath.Join(dir, "events.ndjson")); strings.Contains(data, "expired-secret") {
		t.Fatalf("expired history retained: %s", data)
	}
}
