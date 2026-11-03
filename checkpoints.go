package kosa8

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxCheckpointsPerSandbox is how many checkpoints are kept per sandbox (pkg/checkpoint MaxPerSandbox).
const MaxCheckpointsPerSandbox = 10

// Checkpoint is one restore point: a snapshot with the id <sandbox>-ckpt-<n>.
type Checkpoint struct {
	ID      string    `json:"id"`
	Sandbox string    `json:"sandbox"`
	Label   string    `json:"label"`
	Auto    bool      `json:"auto,omitempty"`
	Created time.Time `json:"created"`
}

type storeData map[string][]Checkpoint

// CheckpointStore keeps checkpoint stacks.
type CheckpointStore interface {
	Load() (map[string][]Checkpoint, error)
	// Update runs fn on the stacks and saves what it leaves, serialised against other updates.
	Update(fn func(map[string][]Checkpoint) error) error
}

// MemoryCheckpointStore keeps stacks in memory; nothing is shared with the CLI.
type MemoryCheckpointStore struct {
	mu   sync.Mutex
	data storeData
}

func clone(d storeData) storeData {
	out := storeData{}
	for k, v := range d {
		out[k] = append([]Checkpoint(nil), v...)
	}
	return out
}

func (m *MemoryCheckpointStore) Load() (map[string][]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.data), nil
}

func (m *MemoryCheckpointStore) Update(fn func(map[string][]Checkpoint) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := clone(m.data)
	if err := fn(d); err != nil {
		return err
	}
	m.data = d
	return nil
}

// FileCheckpointStore is the store the kosa8 CLI uses: ~/.kosa8/checkpoints.json
// (0600, rewritten atomically), locked with the same flock on checkpoints.json.lock,
// so `kosa8 rewind` sees checkpoints taken here and the other way round.
type FileCheckpointStore struct {
	Path string
	mu   sync.Mutex
}

// NewFileCheckpointStore returns the CLI's store, or one at path.
func NewFileCheckpointStore(path string) *FileCheckpointStore {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kosa8", "checkpoints.json")
	}
	return &FileCheckpointStore{Path: path}
}

func (f *FileCheckpointStore) Load() (map[string][]Checkpoint, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return storeData{}, nil
	}
	if err != nil {
		return nil, err
	}
	d := storeData{}
	if json.Unmarshal(b, &d) != nil {
		return storeData{}, nil // a corrupt store must not make the sandbox unusable
	}
	return d, nil
}

func (f *FileCheckpointStore) Update(fn func(map[string][]Checkpoint) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(f.Path + ".lock")
	if err != nil {
		return fmt.Errorf("lock the checkpoint store: %w", err)
	}
	defer unlock()
	d, err := f.Load()
	if err != nil {
		return err
	}
	if err := fn(d); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func ckptErr(format string, a ...any) error {
	return &Error{Kind: KindCheckpoint, Message: fmt.Sprintf(format, a...)}
}

func validSandbox(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return ckptErr("invalid sandbox name %q", name)
	}
	return nil
}

var seqRe = regexp.MustCompile(`^(\d+)`)

func seqOf(id, sandbox string) int {
	pre := sandbox + "-ckpt-"
	if !strings.HasPrefix(id, pre) {
		return 0
	}
	m := seqRe.FindString(id[len(pre):])
	n, _ := strconv.Atoi(m)
	return n
}

// checkpointBook is the stack logic of pkg/checkpoint over any store.
type checkpointBook struct{ store CheckpointStore }

func (b checkpointBook) nextID(sandbox string) (string, []string, error) {
	if err := validSandbox(sandbox); err != nil {
		return "", nil, err
	}
	d, err := b.store.Load()
	if err != nil {
		return "", nil, err
	}
	list := d[sandbox]
	n := len(list) + 1
	for _, c := range list {
		if v := seqOf(c.ID, sandbox); v >= n {
			n = v + 1
		}
	}
	var evict []string
	if over := len(list) + 1 - MaxCheckpointsPerSandbox; over > 0 {
		for _, c := range list[:over] {
			evict = append(evict, c.ID)
		}
	}
	return fmt.Sprintf("%s-ckpt-%d", sandbox, n), evict, nil
}

func (b checkpointBook) record(c Checkpoint, evicted []string) (Checkpoint, error) {
	if err := validSandbox(c.Sandbox); err != nil {
		return c, err
	}
	if c.Created.IsZero() {
		c.Created = time.Now().UTC()
	}
	drop := map[string]bool{}
	for _, e := range evicted {
		drop[e] = true
	}
	return c, b.store.Update(func(d map[string][]Checkpoint) error {
		var keep []Checkpoint
		for _, e := range d[c.Sandbox] {
			if !drop[e.ID] {
				keep = append(keep, e)
			}
		}
		d[c.Sandbox] = append(keep, c)
		return nil
	})
}

func (b checkpointBook) list(sandbox string) ([]Checkpoint, error) {
	d, err := b.store.Load()
	if err != nil {
		return nil, err
	}
	out := append([]Checkpoint(nil), d[sandbox]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (b checkpointBook) latest(sandbox string) (Checkpoint, error) {
	l, err := b.list(sandbox)
	if err != nil {
		return Checkpoint{}, err
	}
	if len(l) == 0 {
		return Checkpoint{}, ckptErr("no checkpoints for %q — take one with Checkpoint or `kosa8 checkpoint %s`", sandbox, sandbox)
	}
	return l[len(l)-1], nil
}

func (b checkpointBook) resolve(sandbox, ref string) (Checkpoint, error) {
	l, err := b.list(sandbox)
	if err != nil {
		return Checkpoint{}, err
	}
	for i := len(l) - 1; i >= 0; i-- {
		if l[i].ID == ref || strings.EqualFold(l[i].Label, ref) {
			return l[i], nil
		}
	}
	return Checkpoint{}, ckptErr("no checkpoint %q for sandbox %q", ref, sandbox)
}

func (b checkpointBook) pop(sandbox, id string) ([]string, error) {
	var orphaned []string
	err := b.store.Update(func(d map[string][]Checkpoint) error {
		list := d[sandbox]
		for i, c := range list {
			if c.ID == id {
				for _, o := range list[i+1:] {
					orphaned = append(orphaned, o.ID)
				}
				d[sandbox] = list[:i+1]
				return nil
			}
		}
		return ckptErr("no checkpoint %q for sandbox %q", id, sandbox)
	})
	return orphaned, err
}

func (b checkpointBook) forget(sandbox string) ([]string, error) {
	var ids []string
	err := b.store.Update(func(d map[string][]Checkpoint) error {
		for _, c := range d[sandbox] {
			ids = append(ids, c.ID)
		}
		delete(d, sandbox)
		return nil
	})
	return ids, err
}
