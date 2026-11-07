package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/delaanthonio/lease/internal/paths"
	"github.com/delaanthonio/lease/internal/proc"
)

// FileStore keeps metadata as small files under one root directory:
//
//	holders/<resource>/<slot>.json   one holder record per slot
//	waiters/<resource>/<id>.json     one waiter ticket per waiting caller
//	events.ndjson                    append-only history, one JSON per line
type FileStore struct {
	root string
}

// NewFileStore returns a FileStore rooted at dir. The directory is created
// on first use, not at construction.
func NewFileStore(dir string) *FileStore {
	return &FileStore{root: dir}
}

// NameValid reports whether a resource name can be used as a file name.
// Names may contain letters, digits, dashes, dots, and underscores.
func NameValid(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

func (s *FileStore) holderPath(resource string, slot int) string {
	return filepath.Join(s.root, "holders", paths.ResourceComponent(resource), strconv.Itoa(slot)+".json")
}

func (s *FileStore) waiterPath(resource, id string) string {
	return filepath.Join(s.root, "waiters", paths.ResourceComponent(resource), id+".json")
}

func (s *FileStore) eventsPath() string {
	return filepath.Join(s.root, "events.ndjson")
}

func (s *FileStore) Holder(resource string, slot int) (*Holder, error) {
	raw, err := os.ReadFile(s.holderPath(resource, slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h Holder
	if err := json.Unmarshal(raw, &h); err != nil {
		// A partially written record is not worth failing over; treat it
		// as absent so a new caller can take the slot.
		return nil, nil
	}
	return &h, nil
}

func (s *FileStore) Holders() ([]Holder, error) {
	dir := filepath.Join(s.root, "holders")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Holder
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		resource := entry.Name()
		slots, err := os.ReadDir(filepath.Join(dir, resource))
		if err != nil {
			return nil, err
		}
		for _, sf := range slots {
			if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, resource, sf.Name()))
			if err != nil {
				continue // A record removed concurrently is skipped.
			}
			var h Holder
			if err := json.Unmarshal(raw, &h); err != nil {
				continue // A partially written record is skipped.
			}
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Slot < out[j].Slot
	})
	return out, nil
}

func (s *FileStore) SetHolder(h *Holder) error {
	if !NameValid(h.Resource) {
		return fmt.Errorf("invalid resource name %q", h.Resource)
	}
	path := s.holderPath(h.Resource, h.Slot)
	if err := paths.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeFileSync(path, raw)
}

func (s *FileStore) ClearHolder(resource string, slot int) error {
	err := os.Remove(s.holderPath(resource, slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *FileStore) Waiters(resource string) ([]Waiter, error) {
	dir := filepath.Join(s.root, "waiters", paths.ResourceComponent(resource))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Waiter
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue // A ticket removed concurrently is not an error here.
		}
		var w Waiter
		if err := json.Unmarshal(raw, &w); err != nil {
			continue // A partially written ticket is skipped.
		}
		if w.PID > 0 && proc.HolderIsStale(w.PID, w.ProcStart) != nil {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}

func (s *FileStore) EnqueueWaiter(w *Waiter) error {
	if !NameValid(w.Resource) {
		return fmt.Errorf("invalid resource name %q", w.Resource)
	}
	path := s.waiterPath(w.Resource, w.ID)
	if err := paths.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return writeFileSync(path, raw)
}

func (s *FileStore) DropWaiter(id string) error {
	// Tickets are stored per resource; a drop by ID alone has to look in
	// every resource directory.
	dir := filepath.Join(s.root, "waiters")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := id + ".json"
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *FileStore) AppendEvent(e Event) error {
	return s.AppendEventContext(context.Background(), e, false)
}

func (s *FileStore) AppendEventContext(ctx context.Context, e Event, noWait bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if err := paths.MkdirAll(s.root); err != nil {
		return err
	}
	lf, err := paths.OpenFile(filepath.Join(s.root, "events.lock"))
	if err != nil {
		return err
	}
	defer lf.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if noWait {
			return ErrHistoryBusy
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	if paths.Shared {
		info, err := os.Stat(s.eventsPath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode().Perm() != 0660 {
			return fmt.Errorf("private history cannot be used in shared mode; choose a new shared state directory")
		}
	}
	events, err := s.readEvents()
	if err != nil {
		return err
	}
	events = append(events, e)
	return s.writeEvents(events)
}

func (s *FileStore) writeEvents(events []Event) error {
	return s.writeEventsMode(retainedEvents(events), paths.FileMode())
}

func retainedEvents(events []Event) []Event {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	kept := events[:0]
	for _, event := range events {
		if !event.At.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	if len(kept) > 1000 {
		kept = kept[len(kept)-1000:]
	}
	return kept
}

func (s *FileStore) writeEventsMode(kept []Event, mode os.FileMode) error {
	var b strings.Builder
	for _, event := range kept {
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return writeFileSyncMode(s.eventsPath(), []byte(b.String()), mode)
}

func (s *FileStore) Events() ([]Event, error) {
	events, err := s.readEvents()
	if err != nil {
		return nil, err
	}
	kept := retainedEvents(append([]Event(nil), events...))
	if len(kept) == len(events) {
		return kept, nil
	}
	lf, err := paths.OpenFile(filepath.Join(s.root, "events.lock"))
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return kept, nil
		}
		return nil, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	events, err = s.readEvents()
	if err != nil {
		return nil, err
	}
	kept = retainedEvents(append([]Event(nil), events...))
	if len(kept) != len(events) {
		info, err := os.Stat(s.eventsPath())
		if err != nil {
			return nil, err
		}
		if err := s.writeEventsMode(kept, info.Mode().Perm()); err != nil {
			return nil, err
		}
	}
	return kept, nil
}

func (s *FileStore) readEvents() ([]Event, error) {
	raw, err := os.ReadFile(s.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // Skip a torn final line rather than failing reads.
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *FileStore) Resources() ([]string, error) {
	seen := map[string]bool{}
	for _, sub := range []string{"holders", "waiters"} {
		dir := filepath.Join(s.root, sub)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name, err := paths.ResourceName(entry.Name())
			if err == nil && NameValid(name) && paths.ResourceComponent(name) == entry.Name() {
				seen[name] = true
			}
		}
	}
	events, err := s.Events()
	if err != nil {
		return nil, err
	}
	{
		for _, e := range events {
			if e.Resource != "" {
				seen[e.Resource] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// writeFileSync writes the file atomically: temp file, flush, rename, so a
// reader never sees a half-written record.
func writeFileSync(path string, raw []byte) error {
	return writeFileSyncMode(path, raw, paths.FileMode())
}

func writeFileSyncMode(path string, raw []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
