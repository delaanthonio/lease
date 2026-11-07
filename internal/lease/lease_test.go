package lease

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/delaanthonio/lease/internal/config"
	"github.com/delaanthonio/lease/internal/identity"
	"github.com/delaanthonio/lease/internal/lock"
	"github.com/delaanthonio/lease/internal/proc"
	"github.com/delaanthonio/lease/internal/store"
)

// fixture gives one test a shared state directory and in-memory store, so
// every caller in the test contends for the same real lock files while
// metadata flows through the Store interface.
type fixture struct {
	stateDir string
	store    store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{stateDir: t.TempDir(), store: store.NewMemStore()}
}

func (f *fixture) options() *Options {
	return &Options{
		StateDir:     f.stateDir,
		Store:        f.store,
		Identity:     identity.Identity{User: "tester"},
		Now:          func() time.Time { return time.Now().UTC() },
		PollInterval: 5 * time.Millisecond,
	}
}

func busyResource(t *testing.T, err error) *BusyError {
	t.Helper()
	if err == nil {
		t.Fatal("acquire succeeded, want busy")
	}
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want *BusyError", err)
	}
	return busy
}

// selfHolder is what a holder record for this test process looks like.
func selfHolder(resource string, slot int) *store.Holder {
	pid := os.Getpid()
	return &store.Holder{
		Resource:   resource,
		Slot:       slot,
		PID:        pid,
		ProcStart:  proc.Fingerprint(pid),
		AcquiredAt: time.Now().UTC(),
	}
}

func TestAcquireAndRelease(t *testing.T) {
	f := newFixture(t)
	acq, err := f.options().Acquire([]string{"db"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The holder record names this process.
	h, err := f.store.Holder("db", 1)
	if err != nil || h == nil {
		t.Fatalf("holder record missing: %v, %+v", err, h)
	}
	if h.PID != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", h.PID, os.Getpid())
	}

	// A second caller of the single-slot resource is busy (no-wait).
	noWait := f.options()
	noWait.Wait = time.Nanosecond
	_, err = noWait.Acquire([]string{"db"})
	busy := busyResource(t, err)
	if busy.Resource != "db" || busy.Slot != 1 {
		t.Fatalf("busy = %s/%d, want db/1", busy.Resource, busy.Slot)
	}
	if busy.Holder == nil || busy.Holder.PID != os.Getpid() {
		t.Fatalf("busy holder does not name the first caller: %+v", busy.Holder)
	}

	acq.Release()
	if h, _ := f.store.Holder("db", 1); h != nil {
		t.Fatalf("holder record survived release: %+v", h)
	}
	// After release the slot is free again, and the waiter ticket is gone.
	if _, err := f.options().Acquire([]string{"db"}); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if w, _ := f.store.Waiters("db"); len(w) != 0 {
		t.Fatalf("waiter tickets survived: %+v", w)
	}
}

func TestAcquireMultipleResourcesSortedOrder(t *testing.T) {
	f := newFixture(t)
	acq, err := f.options().Acquire([]string{"zeta", "alpha", "mid"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer acq.Release()
	for _, name := range []string{"alpha", "mid", "zeta"} {
		if acq.SlotOf(name) != 1 {
			t.Errorf("SlotOf(%q) = %d, want 1", name, acq.SlotOf(name))
		}
	}
}

func TestAcquireMultipleSlotsOfOneResource(t *testing.T) {
	f := newFixture(t)
	opts := f.options()
	opts.Config = &config.Config{Resources: map[string]config.Resource{"sim": {Slots: 2}}}
	opts.Requested = 2
	acq, err := opts.Acquire([]string{"sim"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer acq.Release()
	if h, _ := f.store.Holder("sim", 2); h == nil {
		t.Fatal("slot 2 was not taken")
	}
	// Both slots are taken, so another caller is busy (no-wait).
	noWait := f.options()
	noWait.Wait = time.Nanosecond
	_, err = noWait.Acquire([]string{"sim"})
	busy := busyResource(t, err)
	if busy.Slot != 1 && busy.Slot != 2 {
		t.Fatalf("busy slot = %d, want 1 or 2", busy.Slot)
	}
}

func TestAcquireBlocksThenSucceedsWhenReleased(t *testing.T) {
	f := newFixture(t)
	acq, err := f.options().Acquire([]string{"db"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	waiter := f.options()
	waiter.Wait = 10 * time.Second
	done := make(chan error, 1)
	go func() {
		got, aerr := waiter.Acquire([]string{"db"})
		if aerr == nil {
			got.Release()
		}
		done <- aerr
	}()

	// While waiting, a waiter ticket is visible through the store.
	var sawWaiter bool
	for i := 0; i < 200 && !sawWaiter; i++ {
		time.Sleep(10 * time.Millisecond)
		if w, _ := f.store.Waiters("db"); len(w) > 0 {
			sawWaiter = true
		}
	}
	if !sawWaiter {
		t.Fatal("no waiter ticket appeared while the caller waited")
	}

	acq.Release()
	select {
	case aerr := <-done:
		if aerr != nil {
			t.Fatalf("waiter failed: %v", aerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired after release")
	}
}

func TestNoWaitFailsImmediately(t *testing.T) {
	f := newFixture(t)
	acq, err := f.options().Acquire([]string{"db"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer acq.Release()

	waiter := f.options()
	waiter.Wait = time.Nanosecond
	start := time.Now()
	_, err = waiter.Acquire([]string{"db"})
	busy := busyResource(t, err)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("no-wait took %s, must fail immediately", elapsed)
	}
	if busy.Holder == nil {
		t.Fatal("busy error does not name the holder")
	}
	// The failed caller left no waiter ticket behind.
	if w, _ := f.store.Waiters("db"); len(w) != 0 {
		t.Fatalf("failed caller left waiter tickets: %+v", w)
	}
}

func TestWaitTimeoutReleasesPartialAcquisition(t *testing.T) {
	f := newFixture(t)
	cfg := &config.Config{Resources: map[string]config.Resource{"a": {Slots: 1}}}
	holder := f.options()
	holder.Config = cfg
	acq, err := holder.Acquire([]string{"b"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer acq.Release()

	// The waiter wants two resources: it takes the free one, then times
	// out on the held one. Both must end up released.
	waiter := f.options()
	waiter.Config = cfg
	waiter.Wait = 100 * time.Millisecond
	_, err = waiter.Acquire([]string{"a", "b"})
	busy := busyResource(t, err)
	if busy.Resource != "b" {
		t.Fatalf("busy resource = %q, want b", busy.Resource)
	}
	probe := f.options()
	probe.Wait = time.Nanosecond
	free, err := probe.Acquire([]string{"a"})
	if err != nil {
		t.Fatalf("partial acquisition of a survived: %v", err)
	}
	free.Release()
	if w, _ := f.store.Waiters("b"); len(w) != 0 {
		t.Fatalf("waiter tickets survived the timeout: %+v", w)
	}
}

func TestStaleHolderIsTakenOver(t *testing.T) {
	f := newFixture(t)
	dead := &store.Holder{
		Resource:   "db",
		Slot:       1,
		PID:        999999999,
		AcquiredAt: time.Now().UTC(),
	}
	if err := f.store.SetHolder(dead); err != nil {
		t.Fatalf("SetHolder: %v", err)
	}

	var notes []string
	opts := f.options()
	opts.Note = func(format string, args ...any) {
		notes = append(notes, fmt.Sprintf(format, args...))
	}
	acq, err := opts.Acquire([]string{"db"})
	if err != nil {
		t.Fatalf("acquire against a stale holder must succeed, got: %v", err)
	}
	defer acq.Release()
	if len(notes) == 0 {
		t.Fatal("takeover was not noted to the user")
	}
	events, _ := f.store.Events()
	if !hasEvent(events, store.EventTakeover) {
		t.Fatalf("no takeover event logged: %+v", events)
	}
	if h, _ := f.store.Holder("db", 1); h == nil || h.PID != os.Getpid() {
		t.Fatalf("holder record not rewritten: %+v", h)
	}
}

func TestTTLExpiryRequiresGoneHolder(t *testing.T) {
	f := newFixture(t)
	// A holder whose process is alive holds the kernel lock and never
	// loses its slot, no matter the TTL: the documented rule is TTL plus
	// unresponsive (a gone process).
	fl, err := lock.OpenSlot(f.stateDir, "db", 1)
	if err != nil {
		t.Fatalf("OpenSlot: %v", err)
	}
	defer fl.Close()
	if ok, err := lock.TryLock(fl); err != nil || !ok {
		t.Fatalf("test could not hold the slot lock: %v", err)
	}
	defer lock.Unlock(fl)
	live := selfHolder("db", 1)
	live.AcquiredAt = time.Now().Add(-2 * time.Hour).UTC()
	if err := f.store.SetHolder(live); err != nil {
		t.Fatalf("SetHolder: %v", err)
	}
	opts := f.options()
	opts.TTL = time.Minute
	opts.Wait = time.Nanosecond
	if _, err := opts.Acquire([]string{"db"}); err == nil {
		t.Fatal("a live holder must keep its slot past the TTL")
	}

	// The same record for a gone pid is evicted: the lock is free, the
	// record is old and its process is unresponsive.
	lock.Unlock(fl)
	gone := *live
	gone.PID = 999999999
	gone.ProcStart = ""
	if err := f.store.SetHolder(&gone); err != nil {
		t.Fatalf("SetHolder: %v", err)
	}
	opts2 := f.options()
	opts2.TTL = time.Minute
	acq, err := opts2.Acquire([]string{"db"})
	if err != nil {
		t.Fatalf("expired gone holder must be evicted, got: %v", err)
	}
	defer acq.Release()
	events, _ := f.store.Events()
	if !hasEvent(events, store.EventTTLExpire) {
		t.Fatalf("no ttl-expired event logged: %+v", events)
	}
}

// hasEvent reports whether the history contains an event of this kind.
func hasEvent(events []store.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestInvalidResourceNames(t *testing.T) {
	f := newFixture(t)
	opts := f.options()
	for _, name := range []string{"", "has space", "a/b", "../escape"} {
		if _, err := opts.Acquire([]string{name}); err == nil {
			t.Errorf("acquire accepted invalid name %q", name)
		}
	}
}

func TestAcquireOnePicksFirstFreeSlot(t *testing.T) {
	f := newFixture(t)
	cfg := &config.Config{Resources: map[string]config.Resource{"sim": {Slots: 2}}}
	opts := f.options()
	opts.Config = cfg
	acq, err := opts.AcquireOne("sim")
	if err != nil {
		t.Fatalf("AcquireOne: %v", err)
	}
	if acq.SlotOf("sim") != 1 {
		t.Fatalf("first acquisition must take slot 1, got %d", acq.SlotOf("sim"))
	}
	second := f.options()
	second.Config = cfg
	acq2, err := second.AcquireOne("sim")
	if err != nil {
		t.Fatalf("second AcquireOne: %v", err)
	}
	defer acq.Release()
	defer acq2.Release()
	if acq2.SlotOf("sim") != 2 {
		t.Fatalf("second acquisition must take slot 2, got %d", acq2.SlotOf("sim"))
	}
}

func TestAcquireOneFailsWhenAllSlotsBusy(t *testing.T) {
	f := newFixture(t)
	acq, err := f.options().AcquireOne("sim")
	if err != nil {
		t.Fatalf("AcquireOne: %v", err)
	}
	defer acq.Release()
	waiter := f.options()
	waiter.Wait = time.Nanosecond
	_, err = waiter.AcquireOne("sim")
	busyResource(t, err)
}
