package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	// events.mu guards appends so concurrent writers produce whole lines.
	eventsMu sync.Mutex
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
	return filepath.Join(s.root, "holders", resource, strconv.Itoa(slot)+".json")
}

func (s *FileStore) waiterPath(resource, id string) string {
	return filepath.Join(s.root, "waiters", resource, id+".json")
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
	dir := filepath.Join(s.root, "waiters", resource)
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
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if err := paths.MkdirAll(s.root); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(s.root, "events.lock"), os.O_CREATE|os.O_RDWR, paths.FileMode())
	if err != nil {
		return err
	}
	defer lf.Close()
	if paths.Shared {
		if err = lf.Chmod(paths.FileMode()); err != nil {
			return err
		}
	}
	if err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	events, err := s.Events()
	if err != nil {
		return err
	}
	events = append(events, e)
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
	var b strings.Builder
	for _, event := range kept {
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return writePrivateFileSync(s.eventsPath(), []byte(b.String()))
}

func writePrivateFileSync(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *FileStore) Events() ([]Event, error) {
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
			if entry.IsDir() {
				seen[entry.Name()] = true
			}
		}
	}
	if events, err := s.Events(); err == nil {
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(paths.FileMode()); err != nil {
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
