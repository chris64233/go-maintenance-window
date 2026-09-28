package maintenance

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Snapshot is the persisted state of an engine: resource registry (including
// current occupancy), live requests and the final-record history.
type Snapshot struct {
	Resources map[string]*Resource `json:"resources"`
	Requests  map[string]*Request  `json:"requests"`
	History   []Record             `json:"history"`
}

// Store persists engine snapshots.
type Store interface {
	// Load returns an empty Snapshot when no state has been saved yet.
	Load() (Snapshot, error)
	// Save replaces the whole persisted state.
	Save(snap Snapshot) error
}

// FileStore persists snapshots as a single JSON file. Every save writes a
// sibling temporary file and renames it into place, so a crash in the middle
// of a write never leaves a torn state file behind.
type FileStore struct {
	path string
}

// NewFileStore creates a store backed by the file at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load implements Store.
func (s *FileStore) Load() (Snapshot, error) {
	var snap Snapshot
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{Resources: map[string]*Resource{}, Requests: map[string]*Request{}}, nil
	}
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, err
	}
	if snap.Resources == nil {
		snap.Resources = map[string]*Resource{}
	}
	if snap.Requests == nil {
		snap.Requests = map[string]*Request{}
	}
	return snap, nil
}

// Save implements Store.
func (s *FileStore) Save(snap Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// MemoryStore keeps the latest snapshot in memory, decoupling its copy from
// the engine via a JSON round-trip. It is mainly useful for tests and
// ephemeral runs.
type MemoryStore struct {
	mu   sync.Mutex
	data []byte
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// Load implements Store.
func (s *MemoryStore) Load() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap Snapshot
	if s.data == nil {
		return Snapshot{Resources: map[string]*Resource{}, Requests: map[string]*Request{}}, nil
	}
	if err := json.Unmarshal(s.data, &snap); err != nil {
		return Snapshot{}, err
	}
	if snap.Resources == nil {
		snap.Resources = map[string]*Resource{}
	}
	if snap.Requests == nil {
		snap.Requests = map[string]*Request{}
	}
	return snap, nil
}

// Save implements Store.
func (s *MemoryStore) Save(snap Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
	return nil
}
