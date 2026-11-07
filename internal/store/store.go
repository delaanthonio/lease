// Package store defines the metadata interface for lease: holder records,
// waiter tickets, and the append-only event history.
//
// Commands never touch the metadata files directly; they go through the
// Store interface so a future implementation (for example SQLite-backed
// history and stats) can replace the file-backed one without changing the
// commands. The kernel file locks themselves live outside this interface:
// they are held by processes through flock, not recorded by it.
package store

import (
	"time"
)

// Event kinds written to the append-only history.
const (
	EventAcquire      = "acquire"
	EventRelease      = "release"
	EventTakeover     = "takeover"
	EventTTLExpire    = "ttl-expired"
	EventForceRelease = "force-release"
	EventWait         = "wait"
	EventWaitEnd      = "wait-end"
)

// Holder records who currently holds one slot of a resource. PID is the
// process holding the kernel file lock for the slot. ProcStart identifies
// that specific process incarnation, so a recycled PID is not mistaken for
// the recorded holder.
type Holder struct {
	Resource   string    `json:"resource"`
	Slot       int       `json:"slot"`
	Token      string    `json:"token,omitempty"`
	PID        int       `json:"pid"`
	ProcStart  string    `json:"proc_start,omitempty"`
	User       string    `json:"user,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
	CIRunID    string    `json:"ci_run_id,omitempty"`
	CIJob      string    `json:"ci_job,omitempty"`
	Command    string    `json:"command,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// Waiter is a caller queued for a resource while it waits for slots to
// free up. Tickets are advisory: they exist so status can show who is
// waiting and roughly since when.
type Waiter struct {
	PID       int       `json:"pid,omitempty"`
	ProcStart string    `json:"proc_start,omitempty"`
	ID        string    `json:"id"`
	Resource  string    `json:"resource"`
	User      string    `json:"user,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	CIRunID   string    `json:"ci_run_id,omitempty"`
	CIJob     string    `json:"ci_job,omitempty"`
	Command   string    `json:"command,omitempty"`
	Since     time.Time `json:"since"`
}

// Event is one entry in the append-only history, including every
// force-release with its reason and the previous holder.
type Event struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Resource string    `json:"resource,omitempty"`
	Slot     int       `json:"slot,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Store is the metadata interface behind every lease command. The
// file-backed implementation in this package is the only one in version 1.
type Store interface {
	// Holder returns the holder record for one slot of a resource, or nil
	// when the slot has no record.
	Holder(resource string, slot int) (*Holder, error)
	// Holders lists every current holder record across all resources,
	// ordered by resource then slot.
	Holders() ([]Holder, error)
	// SetHolder writes the holder record for one slot, replacing any
	// previous record for that slot.
	SetHolder(h *Holder) error
	// ClearHolder removes the holder record for one slot. Clearing a slot
	// with no record is not an error.
	ClearHolder(resource string, slot int) error

	// Waiters lists the waiter tickets for a resource, oldest first.
	Waiters(resource string) ([]Waiter, error)
	// EnqueueWaiter records a waiter ticket.
	EnqueueWaiter(w *Waiter) error
	// DropWaiter removes a waiter ticket by ID. Dropping an unknown ID is
	// not an error.
	DropWaiter(id string) error

	// AppendEvent adds one entry to the append-only history.
	AppendEvent(e Event) error
	// Events returns the history, oldest first.
	Events() ([]Event, error)

	// Resources lists every resource that has at least one holder record,
	// waiter ticket, or history event.
	Resources() ([]string, error)
}
