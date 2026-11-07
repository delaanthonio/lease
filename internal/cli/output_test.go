package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/delaanthonio/lease/internal/lease"
	"github.com/delaanthonio/lease/internal/store"
)

func sampleViews(now time.Time) []lease.ResourceView {
	// A live holder: this test process itself, so the record is current.
	held := &store.Holder{
		Resource:   "db",
		Slot:       1,
		PID:        os.Getpid(),
		TaskID:     "task-7",
		Command:    "pg_restore data",
		AcquiredAt: now.Add(-5 * time.Minute),
	}
	// A gone holder: its record is stale.
	stale := &store.Holder{
		Resource:   "runner",
		Slot:       1,
		PID:        999999999,
		AcquiredAt: now.Add(-2 * time.Hour),
	}
	return []lease.ResourceView{
		{Resource: "db", Total: 2, Used: 1, Holders: []*store.Holder{held, nil}, Waiting: 3},
		{Resource: "ports", Total: 1, Used: 0, Holders: []*store.Holder{nil}, Waiting: 0},
		{Resource: "runner", Total: 1, Used: 1, Holders: []*store.Holder{stale}, Waiting: 0},
	}
}

func TestRenderJSONIsCompactStableAndComplete(t *testing.T) {
	now := time.Now().UTC()
	var b strings.Builder
	if err := RenderJSON(&b, sampleViews(now)); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	// Compact: one line.
	out := b.String()
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("JSON output is not one compact line: %q", out)
	}
	var views []struct {
		Resource string `json:"resource"`
		Total    int    `json:"total"`
		Used     int    `json:"used"`
		Waiting  int    `json:"waiting"`
		Holders  []struct {
			Slot   int    `json:"slot"`
			Holder string `json:"holder"`
			Since  string `json:"since"`
			Stale  bool   `json:"stale"`
		} `json:"holders"`
	}
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	// Independent expectation, assembled by hand from the sample records.
	if len(views) != 3 {
		t.Fatalf("resource count = %d, want 3", len(views))
	}
	for i, want := range []string{"db", "ports", "runner"} {
		if views[i].Resource != want {
			t.Fatalf("resource %d = %q, want %q", i, views[i].Resource, want)
		}
	}
	if views[0].Total != 2 || views[0].Used != 1 || views[0].Waiting != 3 {
		t.Fatalf("db counts wrong: %+v", views[0])
	}
	if len(views[0].Holders) != 1 {
		t.Fatalf("only held slots appear in holders: %+v", views[0].Holders)
	}
	h := views[0].Holders[0]
	if h.Slot != 1 || !strings.Contains(h.Holder, "task:task-7") || h.Stale {
		t.Fatalf("holder wrong: %+v", h)
	}
	wantSince := now.Add(-5 * time.Minute).Format(time.RFC3339)
	if h.Since != wantSince {
		t.Fatalf("since = %q, want %q", h.Since, wantSince)
	}
	// The resource with no holders still appears, with an empty list.
	if views[1].Used != 0 || len(views[1].Holders) != 0 {
		t.Fatalf("free resource wrong: %+v", views[1])
	}
	// A stale holder is flagged in JSON.
	if !views[2].Holders[0].Stale {
		t.Fatalf("gone holder not flagged stale: %+v", views[2].Holders[0])
	}
	// Stability: the same input renders identically twice.
	var again strings.Builder
	if err := RenderJSON(&again, sampleViews(now)); err != nil {
		t.Fatalf("RenderJSON again: %v", err)
	}
	if again.String() != out {
		t.Fatal("JSON output is not deterministic for the same input")
	}
}

func TestRenderTableShowsHolderSinceAndSlots(t *testing.T) {
	now := time.Now().UTC()
	var b strings.Builder
	RenderTable(&b, sampleViews(now), now)
	out := b.String()
	for _, want := range []string{
		"RESOURCE", "HOLDER", "SINCE", "SLOTS", "WAITING",
		"task:task-7", "5m", "1/2", "ports", "0/1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// A stale holder is marked as such.
	if !strings.Contains(out, "(stale)") {
		t.Errorf("stale holder not marked:\n%s", out)
	}
}

func TestRenderTableEmpty(t *testing.T) {
	var b strings.Builder
	RenderTable(&b, nil, time.Now())
	if !strings.Contains(b.String(), "no resources") {
		t.Fatalf("empty view output: %q", b.String())
	}
}

func TestHumanizeSince(t *testing.T) {
	now := time.Now()
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{25 * time.Hour, "1d1h"},
	}
	for _, c := range cases {
		if got := humanizeSince(now.Add(-c.age), now); got != c.want {
			t.Errorf("humanizeSince(%s) = %q, want %q", c.age, got, c.want)
		}
	}
	if got := humanizeSince(time.Time{}, now); got != "-" {
		t.Errorf("zero time = %q, want -", got)
	}
}
