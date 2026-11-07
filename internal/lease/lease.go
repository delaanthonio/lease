// Package lease orchestrates acquiring and releasing named resource
// slots. Callers request slots for one or more resources; every resource
// unit is acquired in one fixed global order (resource name, then slot
// number), which makes deadlock between lease callers impossible.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/delaanthonio/lease/internal/config"
	"github.com/delaanthonio/lease/internal/identity"
	"github.com/delaanthonio/lease/internal/lock"
	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/proc"
	"github.com/delaanthonio/lease/internal/store"
)

// DefaultPollInterval is how often a waiting caller re-attempts busy slots.
const DefaultPollInterval = 50 * time.Millisecond

// ExitBusy is the exit code used when a lock cannot be taken because the
// resource is busy or the wait elapsed.
const ExitBusy = 75

// ErrBusy reports that a resource could not be acquired. Detail names the
// current holder when one is known.
type BusyError struct {
	Resource string
	Slot     int
	Holder   *store.Holder
}

func (e *BusyError) Error() string {
	if e.Holder == nil {
		return fmt.Sprintf("resource %q is busy (holder unknown)", e.Resource)
	}
	return fmt.Sprintf("resource %q slot %d is held by %s since %s",
		e.Resource, e.Slot, DescribeHolder(e.Holder), e.Holder.AcquiredAt.Format(time.RFC3339))
}

// Options configures one acquisition.
type Options struct {
	Context context.Context
	// StateDir is the machine-wide directory holding lock and metadata files.
	StateDir string
	// Store holds holder records, waiter tickets, and history.
	Store store.Store
	// Config resolves slot counts; nil means defaults only.
	Config *config.Config
	// Identity labels the holder in records and history.
	Identity identity.Identity
	// Requested is how many slots to take per resource; zero means use
	// the configured slot count.
	Requested int
	// Wait bounds how long busy slots are retried; zero waits forever.
	Wait time.Duration
	// TTL bounds how long a gone holder's record may keep blocking a slot.
	TTL time.Duration
	// Token is recorded in the holder record when set (manual acquire).
	Token string
	// Now is injectable for tests.
	Now func() time.Time
	// Note reports takeovers and similar observations to the user.
	Note func(format string, args ...any)
	// PollInterval is the busy retry period; zero uses DefaultPollInterval.
	PollInterval time.Duration
	// Command describes what this caller runs, recorded in holder records
	// and history (for example the child argv, or "lease acquire NAME").
	Command string
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) note(format string, args ...any) {
	if o.Note != nil {
		o.Note(format, args...)
	}
}

func (o *Options) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return DefaultPollInterval
}

// Capacity returns how many slots a resource has in total, from the
// config; unknown names have one exclusive slot.
func (o *Options) Capacity(resource string) int {
	if o.Config != nil {
		return o.Config.Slots(resource)
	}
	return config.DefaultSlots
}

// want returns how many slots this acquisition requests of each resource.
func (o *Options) want() int {
	if o.Requested > 0 {
		return o.Requested
	}
	return 1
}

// unit is one acquisition target: one slot of one resource.
type unit struct {
	resource string
	slot     int
}

// Acquired holds everything taken by a successful Acquire.
type Acquired struct {
	ctx    context.Context
	opts   *Options
	units  []unit
	files  map[unit]*os.File
	waiter map[string]string // resource -> waiter ticket id
	waited bool
}

// ValidateNames rejects resource names that cannot be stored as files.
func ValidateNames(names []string) error {
	for _, name := range names {
		if !store.NameValid(name) {
			return fmt.Errorf("invalid resource name %q: use letters, digits, dashes, dots, or underscores", name)
		}
	}
	return nil
}

func (o *Options) acquisitionContext() (context.Context, context.CancelFunc) {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if o.Wait > time.Nanosecond {
		return context.WithTimeout(ctx, o.Wait)
	}
	return context.WithCancel(ctx)
}

func (o *Options) acquisitionError(a *Acquired, resource string, deadline time.Time, err error) error {
	if a.ctx.Err() == context.Canceled {
		return context.Canceled
	}
	if a.ctx.Err() == context.DeadlineExceeded || (o.Wait > time.Nanosecond && !deadline.IsZero() && !o.now().Before(deadline)) {
		return o.busyAt(resource)
	}
	return err
}

// Acquire takes the requested slots of every named resource, in fixed
// sorted order, waiting for busy slots per the options. On failure it
// releases everything already taken and returns *BusyError.
//
// Each resource is taken all-or-nothing: the caller holds no partial set
// of a resource while waiting, which keeps counted slots deadlock-free
// between callers. Resources themselves are taken in sorted order, which
// keeps multi-resource callers deadlock-free.
func (o *Options) Acquire(names []string) (*Acquired, error) {
	if err := ValidateNames(names); err != nil {
		return nil, err
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return nil, fmt.Errorf("duplicate resource %q", sorted[i])
		}
	}

	ctx, cancel := o.acquisitionContext()
	defer cancel()
	a := &Acquired{
		ctx:    ctx,
		opts:   o,
		files:  map[unit]*os.File{},
		waiter: map[string]string{},
	}
	var deadline time.Time
	if o.Wait > 0 {
		deadline = o.now().Add(o.Wait)
	}
	for _, resource := range sorted {
		err := o.acquireResource(a, resource, deadline)
		if err != nil {
			err = o.acquisitionError(a, resource, deadline, err)
			cancel()
			a.release(ctx)
			return nil, err
		}
	}
	if len(sorted) > 0 {
		if err := o.acquisitionError(a, sorted[len(sorted)-1], deadline, nil); err != nil {
			cancel()
			a.release(ctx)
			return nil, err
		}
	}
	return a, nil
}

// AcquireOne takes a single slot of a resource, whichever index is free
// first. It is used for manual acquisition, where each keeper process
// holds exactly one slot.
func (o *Options) AcquireOne(resource string) (*Acquired, error) {
	if err := ValidateNames([]string{resource}); err != nil {
		return nil, err
	}
	ctx, cancel := o.acquisitionContext()
	defer cancel()
	a := &Acquired{
		ctx:    ctx,
		opts:   o,
		files:  map[unit]*os.File{},
		waiter: map[string]string{},
	}
	var deadline time.Time
	if o.Wait > 0 {
		deadline = o.now().Add(o.Wait)
	}
	err := o.acquireResource(a, resource, deadline)
	err = o.acquisitionError(a, resource, deadline, err)
	if err != nil {
		cancel()
		a.release(ctx)
		return nil, err
	}
	return a, nil
}

// acquireResource takes `want` slots of one resource all at once. It
// holds no slots while waiting for the set to become available.
func (o *Options) acquireResource(a *Acquired, resource string, deadline time.Time) error {
	total := o.Capacity(resource)
	want := o.want()
	if want > total {
		return fmt.Errorf("resource %q has %d slot(s) but %d were requested", resource, total, want)
	}
	first := true
	for {
		if (!first || a.waited || (len(a.units) > 0 && o.Wait > time.Nanosecond)) && !deadline.IsZero() && !o.now().Before(deadline) {
			return o.busyAt(resource)
		}
		first = false
		if err := a.ctx.Err(); err != nil {
			return err
		}
		// Tentatively lock free slots until the wanted count is reached.
		var taken []unit
		for slot := 1; slot <= total && len(taken) < want; slot++ {
			f, err := lock.OpenSlot(o.StateDir, resource, slot)
			if err != nil {
				a.dropTentative(taken)
				return err
			}
			ok, err := lock.TryLock(f)
			if err != nil {
				f.Close()
				a.dropTentative(taken)
				return err
			}
			if ok {
				u := unit{resource, slot}
				a.files[u] = f
				taken = append(taken, u)
				continue
			}
			f.Close()
		}
		if len(taken) >= want {
			_, _ = o.Store.Waiters(resource)
			if id := a.waiter[resource]; id != "" {
				_ = o.Store.DropWaiter(id)
				delete(a.waiter, resource)
			}
			a.units = append(a.units, taken...)
			for _, u := range taken {
				o.resolveExistingRecord(a, u)
				if err := a.ctx.Err(); err != nil {
					return err
				}
				if err := o.recordHolder(u); err != nil {
					return err
				}
				if err := store.AppendEventContext(a.ctx, o.Store, store.Event{At: o.now().UTC(), Kind: store.EventAcquire, Resource: u.resource, Slot: u.slot, Actor: o.Identity.Label(), Detail: o.Command}, true); err != nil && !errors.Is(err, store.ErrHistoryBusy) {
					return err
				}
			}
			return nil
		}
		// Not enough slots free together: hold nothing while waiting.
		a.dropTentative(taken)
		a.waited = true
		if !o.waitRound(a, resource, deadline) {
			if err := a.ctx.Err(); err != nil {
				return err
			}
			return o.busyAt(resource)
		}
	}
}

// dropTentative releases slots locked during a scan that did not reach
// the wanted count.
func (a *Acquired) dropTentative(taken []unit) {
	for _, u := range taken {
		if f, ok := a.files[u]; ok {
			_ = lock.Unlock(f)
			_ = f.Close()
			delete(a.files, u)
		}
	}
}

// resolveExistingRecord reconciles a leftover holder record when a slot is
// taken. A stale record (its process is gone) or an expired one is logged
// as a takeover; a record naming a live process on a free slot is an
// inconsistency the new holder resolves, also logged.
func (o *Options) resolveExistingRecord(a *Acquired, u unit) {
	holder, err := o.Store.Holder(u.resource, u.slot)
	if err != nil || holder == nil {
		return
	}
	switch {
	case o.ttlExpired(holder):
		o.recordTakeover(a, u, holder, store.EventTTLExpire, "TTL elapsed and holder unresponsive")
		o.note("slot %d of %q was held past its TTL by %s and is unresponsive; taking over",
			u.slot, u.resource, DescribeHolderForHistory(holder))
	case holderStale(holder):
		o.recordTakeover(a, u, holder, store.EventTakeover, "holder process is gone")
		o.note("slot %d of %q had a stale holder (%s); taking over",
			u.slot, u.resource, DescribeHolderForHistory(holder))
	default:
		o.recordTakeover(a, u, holder, store.EventTakeover, "holder record present but slot lock free")
	}
}

// recordTakeover clears a leftover holder record and appends the takeover
// event.
func (o *Options) recordTakeover(a *Acquired, u unit, holder *store.Holder, kind, reason string) {
	_ = o.Store.ClearHolder(u.resource, u.slot)
	if err := store.AppendEventContext(a.ctx, o.Store, store.Event{
		At: o.now().UTC(), Kind: kind,
		Resource: u.resource, Slot: u.slot,
		Actor: o.Identity.Label(), Reason: reason,
		Detail: DescribeHolderForHistory(holder),
	}, true); err != nil && !errors.Is(err, store.ErrHistoryBusy) {
		o.note("record takeover: %s", err)
	}
}

// waitRound queues a waiter ticket (once per resource) and sleeps until
// the next retry. It reports false when the deadline passed, meaning the
// caller should give up.
func (o *Options) waitRound(a *Acquired, resource string, deadline time.Time) bool {
	if a.waiter[resource] == "" {
		_, _ = o.Store.Waiters(resource)
		id, err := randomID("wait-")
		if err == nil {
			ticket := &store.Waiter{
				ID:        id,
				PID:       os.Getpid(),
				ProcStart: proc.Fingerprint(os.Getpid()),
				Resource:  resource,
				User:      o.Identity.User,
				TaskID:    o.Identity.TaskID,
				CIRunID:   o.Identity.CIRunID,
				CIJob:     o.Identity.CIJob,
				Command:   o.Command,
				Since:     o.now().UTC(),
			}
			if err := o.Store.EnqueueWaiter(ticket); err == nil {
				a.waiter[resource] = id
				if err := store.AppendEventContext(a.ctx, o.Store, store.Event{
					At: o.now().UTC(), Kind: store.EventWait,
					Resource: resource, Actor: o.Identity.Label(),
				}, true); err != nil && !errors.Is(err, store.ErrHistoryBusy) {
					o.note("record wait: %s", err)
				}
			}
		}
	}
	if !deadline.IsZero() && !o.now().Before(deadline) {
		return false
	}
	delay := o.pollInterval()
	if !deadline.IsZero() {
		if remaining := deadline.Sub(o.now()); remaining < delay {
			delay = remaining
		}
	}
	if delay <= 0 {
		return false
	}
	select {
	case <-a.ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// busyAt builds the busy error for a resource, naming the current holder
// of any of its slots when one is recorded.
func (o *Options) busyAt(resource string) error {
	total := o.Capacity(resource)
	for slot := 1; slot <= total; slot++ {
		holder, err := o.Store.Holder(resource, slot)
		if err == nil && holder != nil {
			return &BusyError{Resource: resource, Slot: slot, Holder: holder}
		}
	}
	return &BusyError{Resource: resource}
}

func (o *Options) ttlExpired(h *store.Holder) bool {
	if o.TTL <= 0 {
		return false
	}
	return h.AcquiredAt.Add(o.TTL).Before(o.now()) && holderStale(h)
}

func holderStale(h *store.Holder) bool {
	return proc.HolderIsStale(h.PID, h.ProcStart) != nil
}

func (o *Options) recordHolder(u unit) error {
	pid := os.Getpid()
	h := &store.Holder{
		Resource:   u.resource,
		Slot:       u.slot,
		Token:      o.Token,
		PID:        pid,
		ProcStart:  proc.Fingerprint(pid),
		User:       o.Identity.User,
		TaskID:     o.Identity.TaskID,
		CIRunID:    o.Identity.CIRunID,
		CIJob:      o.Identity.CIJob,
		Command:    o.Command,
		AcquiredAt: o.now().UTC(),
	}
	return o.Store.SetHolder(h)
}

// Release drops holder records, releases the kernel locks, and clears
// waiter tickets. It is safe to call more than once.
func (a *Acquired) Release() {
	ctx := context.Background()
	if a != nil && a.opts.Context != nil {
		ctx = a.opts.Context
	}
	a.release(ctx)
}

func (a *Acquired) release(ctx context.Context) {
	if a == nil {
		return
	}
	var released []unit
	for _, u := range a.units {
		f, ok := a.files[u]
		if !ok {
			continue
		}
		_ = f.Close()
		delete(a.files, u)
		probe, err := lock.OpenSlot(a.opts.StateDir, u.resource, u.slot)
		if err == nil {
			held, lockErr := lock.TryLock(probe)
			if lockErr == nil && held {
				h, readErr := a.opts.Store.Holder(u.resource, u.slot)
				if readErr == nil && h != nil && h.PID == os.Getpid() && h.ProcStart == proc.Fingerprint(os.Getpid()) && h.Token == a.opts.Token {
					_ = a.opts.Store.ClearHolder(u.resource, u.slot)
				}
				_ = lock.Unlock(probe)
			}
			_ = probe.Close()
		}
		released = append(released, u)
	}
	for _, u := range released {
		if err := store.AppendEventContext(ctx, a.opts.Store, store.Event{
			At:       a.opts.now().UTC(),
			Kind:     store.EventRelease,
			Resource: u.resource,
			Slot:     u.slot,
			Actor:    a.opts.Identity.Label(),
		}, true); err != nil && !errors.Is(err, store.ErrHistoryBusy) {
			a.opts.note("record release: %s", err)
		}
	}
	for resource, id := range a.waiter {
		_ = a.opts.Store.DropWaiter(id)
		delete(a.waiter, resource)
	}
}

// Files returns the locked descriptors for inheritance by a resource user.
func (a *Acquired) Files() []*os.File {
	var files []*os.File
	for _, u := range a.units {
		files = append(files, a.files[u])
	}
	return files
}

// SlotOf returns the slot this acquisition took of a resource, or 0 when
// it took none.
func (a *Acquired) SlotOf(resource string) int {
	for _, u := range a.units {
		if u.resource == resource {
			return u.slot
		}
	}
	return 0
}

// Inspect builds a view of resources for status: how many slots exist,
// how many are used, who holds them, and how many callers wait. Unknown
// resources default to one slot.
type ResourceView struct {
	Resource string
	Total    int
	Used     int
	Holders  []*store.Holder // per slot index 1..Total, nil when free
	SlotUsed []bool
	Waiting  int
}

// Inspect probes each resource. It never changes holder records.
func Inspect(stateDir string, st store.Store, cfg *config.Config, resources []string) ([]ResourceView, error) {
	if err := ValidateNames(resources); err != nil {
		return nil, err
	}
	sorted := append([]string(nil), resources...)
	sort.Strings(sorted)
	out := make([]ResourceView, 0, len(sorted))
	for _, resource := range sorted {
		total := config.DefaultSlots
		if cfg != nil {
			total = cfg.Slots(resource)
		}
		view := ResourceView{Resource: resource, Total: total}
		for slot := 1; slot <= total; slot++ {
			used, err := lock.Probe(stateDir, resource, slot)
			if err != nil {
				return nil, err
			}
			holder, err := st.Holder(resource, slot)
			if err != nil {
				return nil, err
			}
			view.SlotUsed = append(view.SlotUsed, used)
			if used {
				view.Used++
				view.Holders = append(view.Holders, holder)
			} else {
				view.Holders = append(view.Holders, nil)
			}
		}
		waiters, err := st.Waiters(resource)
		if err != nil {
			return nil, err
		}
		view.Waiting = len(waiters)
		out = append(out, view)
	}
	return out, nil
}

// DescribeHolder renders a holder record for humans: task id or CI run id
// when present, otherwise user, pid, and command.
func DescribeHolder(h *store.Holder) string {
	id := identity.Identity{TaskID: h.TaskID, CIRunID: h.CIRunID, CIJob: h.CIJob, User: h.User}
	parts := []string{id.Label()}
	if h.TaskID == "" && h.CIRunID == "" && h.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", h.PID))
	}
	if h.Command != "" {
		parts = append(parts, h.Command)
	}
	return strings.Join(parts, " ")
}

func DescribeHolderForHistory(h *store.Holder) string {
	if paths.Shared {
		copy := *h
		copy.Command = ""
		return DescribeHolder(&copy)
	}
	return DescribeHolder(h)
}

// randomID returns a random file-safe identifier with the given prefix.
func randomID(prefix string) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
