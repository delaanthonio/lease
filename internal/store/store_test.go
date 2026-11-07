package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delaanthonio/lease/internal/paths"
)

// storeCases run identical table-driven assertions against both Store
// implementations, so the in-memory fake used by command tests provably
// matches the file-backed one.
func storeCases(t *testing.T) map[string]func() Store {
	t.Helper()
	return map[string]func() Store{
		"mem":  func() Store { return NewMemStore() },
		"file": func() Store { return NewFileStore(t.TempDir()) },
	}
}

func TestHolderLifecycle(t *testing.T) {
	for name, newStore := range storeCases(t) {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			h, err := st.Holder("sim", 1)
			if err != nil || h != nil {
				t.Fatalf("empty store: holder=%v err=%v, want nil/nil", h, err)
			}
			want := &Holder{Resource: "sim", Slot: 1, PID: 42, AcquiredAt: time.Now().UTC().Truncate(time.Second)}
			if err := st.SetHolder(want); err != nil {
				t.Fatalf("SetHolder: %v", err)
			}
			got, err := st.Holder("sim", 1)
			if err != nil {
				t.Fatalf("Holder: %v", err)
			}
			if got.PID != 42 || got.Resource != "sim" || got.Slot != 1 {
				t.Fatalf("got %+v, want pid 42 of sim slot 1", got)
			}
			// A record is replaced, not duplicated.
			want.PID = 43
			if err := st.SetHolder(want); err != nil {
				t.Fatalf("SetHolder replace: %v", err)
			}
			got, _ = st.Holder("sim", 1)
			if got.PID != 43 {
				t.Fatalf("replace failed: got pid %d", got.PID)
			}
			if err := st.ClearHolder("sim", 1); err != nil {
				t.Fatalf("ClearHolder: %v", err)
			}
			// Clearing again is not an error.
			if err := st.ClearHolder("sim", 1); err != nil {
				t.Fatalf("ClearHolder twice: %v", err)
			}
			if h, _ := st.Holder("sim", 1); h != nil {
				t.Fatalf("record survived clear: %+v", h)
			}
		})
	}
}

func TestHoldersListOrder(t *testing.T) {
	for name, newStore := range storeCases(t) {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			for _, h := range []Holder{
				{Resource: "b", Slot: 2},
				{Resource: "a", Slot: 3},
				{Resource: "a", Slot: 1},
			} {
				hh := h
				if err := st.SetHolder(&hh); err != nil {
					t.Fatalf("SetHolder: %v", err)
				}
			}
			holders, err := st.Holders()
			if err != nil {
				t.Fatalf("Holders: %v", err)
			}
			want := []struct {
				r string
				s int
			}{{"a", 1}, {"a", 3}, {"b", 2}}
			if len(holders) != len(want) {
				t.Fatalf("got %d holders, want %d", len(holders), len(want))
			}
			for i, w := range want {
				if holders[i].Resource != w.r || holders[i].Slot != w.s {
					t.Fatalf("holder %d = %s/%d, want %s/%d", i, holders[i].Resource, holders[i].Slot, w.r, w.s)
				}
			}
		})
	}
}

func TestWaitersAndEvents(t *testing.T) {
	for name, newStore := range storeCases(t) {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			earlier := time.Now().Add(-time.Minute).UTC()
			later := time.Now().UTC()
			for _, w := range []Waiter{
				{ID: "w2", Resource: "sim", Since: later},
				{ID: "w1", Resource: "sim", Since: earlier},
			} {
				ww := w
				if err := st.EnqueueWaiter(&ww); err != nil {
					t.Fatalf("EnqueueWaiter: %v", err)
				}
			}
			waiters, err := st.Waiters("sim")
			if err != nil {
				t.Fatalf("Waiters: %v", err)
			}
			if len(waiters) != 2 || waiters[0].ID != "w1" {
				t.Fatalf("waiters not oldest-first: %+v", waiters)
			}
			// Another resource's tickets do not leak in.
			other := &Waiter{ID: "w3", Resource: "ports", Since: earlier}
			if err := st.EnqueueWaiter(other); err != nil {
				t.Fatalf("EnqueueWaiter: %v", err)
			}
			if waiters, _ := st.Waiters("sim"); len(waiters) != 2 {
				t.Fatalf("waiters leaked across resources: %+v", waiters)
			}
			if err := st.DropWaiter("w1"); err != nil {
				t.Fatalf("DropWaiter: %v", err)
			}
			// Dropping an unknown id is not an error.
			if err := st.DropWaiter("nope"); err != nil {
				t.Fatalf("DropWaiter unknown: %v", err)
			}
			waiters, _ = st.Waiters("sim")
			if len(waiters) != 1 || waiters[0].ID != "w2" {
				t.Fatalf("drop failed: %+v", waiters)
			}

			if err := st.AppendEvent(Event{Kind: EventAcquire, Resource: "sim", Slot: 1, Actor: "user:u"}); err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
			if err := st.AppendEvent(Event{Kind: EventRelease, Resource: "sim", Slot: 1, Reason: "done"}); err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
			events, err := st.Events()
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if len(events) != 2 || events[0].Kind != EventAcquire || events[1].Kind != EventRelease {
				t.Fatalf("events not in order: %+v", events)
			}

			resources, err := st.Resources()
			if err != nil {
				t.Fatalf("Resources: %v", err)
			}
			if len(resources) != 2 || resources[0] != "ports" || resources[1] != "sim" {
				t.Fatalf("resources = %v, want [ports sim]", resources)
			}
		})
	}
}

func TestConcurrentAppendEventProducesWholeLines(t *testing.T) {
	dir := t.TempDir()
	st := NewFileStore(dir)
	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := st.AppendEvent(Event{Kind: EventAcquire, Resource: "r", Detail: fmt.Sprintf("w%d-%d", w, i)}); err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	events, err := st.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != writers*perWriter {
		t.Fatalf("got %d events, want %d (a torn line loses data)", len(events), writers*perWriter)
	}
}

func TestNameValid(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"ios-simulator", true},
		{"port_3000", true},
		{"a", true},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"dot.dot", true},
		{string(make([]byte, 129)), false},
	}
	for _, c := range cases {
		if got := NameValid(c.name); got != c.valid {
			t.Errorf("NameValid(%q) = %v, want %v", c.name, got, c.valid)
		}
	}
}

func TestFileStoreSurvivesTornLine(t *testing.T) {
	dir := t.TempDir()
	st := NewFileStore(dir)
	if err := st.AppendEvent(Event{Kind: EventAcquire, Resource: "r"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// Simulate a crash mid-write: a torn final line must be skipped by
	// reads, not turn every later read into an error.
	eventsPath := filepath.Join(dir, "events.ndjson")
	f, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"at":"`); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	events, err := st.Events()
	if err != nil {
		t.Fatalf("Events after torn line: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the 1 complete one", len(events))
	}
}

func TestFileHistoryRetentionAndUnsafeNames(t *testing.T) {
	st := NewFileStore(t.TempDir())
	if NameValid(".") || NameValid("..") {
		t.Fatal("path components accepted as resource names")
	}
	if !NameValid("db.v1") {
		t.Fatal("valid dotted name rejected")
	}
	var lines strings.Builder
	for i := 0; i < 1001; i++ {
		raw, _ := json.Marshal(Event{At: time.Now(), Kind: EventAcquire, Detail: fmt.Sprint(i)})
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	old, _ := json.Marshal(Event{At: time.Now().Add(-31 * 24 * time.Hour), Kind: EventAcquire, Detail: "old"})
	lines.Write(old)
	lines.WriteByte('\n')
	if err := os.WriteFile(st.eventsPath(), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(Event{Kind: EventRelease, Detail: "new"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1000 || events[0].Detail != "2" || events[999].Detail != "new" {
		t.Fatalf("retention: %d events", len(events))
	}
	info, err := os.Stat(st.eventsPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("history mode: %v %v", info, err)
	}
}

func TestSharedModeRefusesPrivateHistory(t *testing.T) {
	previous := paths.Shared
	defer func() { paths.Shared = previous }()
	paths.Shared = false
	st := NewFileStore(t.TempDir())
	if err := st.AppendEvent(Event{Kind: EventAcquire, Detail: "cmd private-secret"}); err != nil {
		t.Fatal(err)
	}
	paths.Shared = true
	if err := st.AppendEvent(Event{Kind: EventAcquire, Detail: "safe"}); err == nil {
		t.Fatal("shared mode published private history")
	}
	info, err := os.Stat(st.eventsPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private history permissions changed: %v %v", info, err)
	}
	events, err := st.Events()
	if err != nil || len(events) != 1 || events[0].Detail != "cmd private-secret" {
		t.Fatalf("private history changed: %+v %v", events, err)
	}
}

func TestHistoryReadPrunesExpiredAndExcessEvents(t *testing.T) {
	st := NewFileStore(t.TempDir())
	var lines strings.Builder
	for i := 0; i < 1002; i++ {
		at := time.Now()
		if i == 0 {
			at = at.Add(-31 * 24 * time.Hour)
		}
		raw, _ := json.Marshal(Event{At: at, Kind: EventAcquire, Detail: fmt.Sprint(i)})
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(st.eventsPath(), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	events, err := st.Events()
	if err != nil || len(events) != 1000 || events[0].Detail != "2" {
		t.Fatalf("events: %d %v", len(events), err)
	}
	raw, err := os.ReadFile(st.eventsPath())
	if err != nil || strings.Count(string(raw), "\n") != 1000 {
		t.Fatalf("persisted history: %v", err)
	}
}
