package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

const JSON = "json"
const SQLite = "sqlite"

// Repository makes state changes atomic: rejected callbacks or failed durable
// writes leave the committed snapshot unchanged. Each returned snapshot is owned
// by the caller. One process owns a data directory for the entire open lifetime.
type Repository interface {
	Snapshot() State
	Update(func(*State) error) error
	Close() error
}
type backend interface {
	save(State) error
	close() error
}
type Store struct {
	mu     sync.RWMutex
	state  State
	disk   backend
	lock   *flock.Flock
	closed bool
}

func Filename(kind string) (string, error) {
	switch kind {
	case JSON:
		return "state.json", nil
	case SQLite:
		return "state.db", nil
	default:
		return "", fmt.Errorf("storage must be json or sqlite")
	}
}
func lockDir(dir string) (*flock.Flock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".tunnel.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	ok, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !ok {
		_ = lock.Close()
		return nil, fmt.Errorf("data directory is in use; stop the Server or other storage command first")
	}
	// Never unlink this file: doing so would let another process lock a new inode.
	return lock, nil
}
func lockExistingDir(dir string) (*flock.Flock, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("data directory is not a directory")
	}
	return lockDir(dir)
}

var sqliteSidecars = []string{"state.db-journal", "state.db-wal", "state.db-shm"}

func rejectSidecars(dir string) error {
	for _, name := range sqliteSidecars {
		if found, err := exists(filepath.Join(dir, name)); err != nil {
			return err
		} else if found {
			return fmt.Errorf("SQLite sidecar %s exists without a usable target; recover/inspect the old directory first", name)
		}
	}
	return nil
}
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Open keeps the legacy JSON default. initialize is called only for a genuinely
// new directory and never when an existing backend cannot be read or validated.
func Open(dir, kind string, initialize func() (State, error)) (*Store, error) {
	name, err := Filename(kind)
	if err != nil {
		return nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{lock: lock}
	success := false
	defer func() {
		if !success {
			if s.disk != nil {
				_ = s.disk.close()
			}
			_ = lock.Close()
		}
	}()
	if err := CheckOperationsRestore(dir); err != nil {
		return nil, err
	}
	jsonExists, err := exists(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	dbExists, err := exists(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, err
	}
	if !dbExists {
		if err := rejectSidecars(dir); err != nil {
			return nil, err
		}
	}
	if kind == JSON && dbExists {
		return nil, fmt.Errorf("state.db exists; select --storage sqlite or restore a backup into a new directory")
	}
	if kind == SQLite && jsonExists && !dbExists {
		return nil, fmt.Errorf("legacy state.json exists; run storage migrate before selecting sqlite")
	}
	hasFile := jsonExists
	if kind == SQLite {
		hasFile = dbExists
	}
	path := filepath.Join(dir, name)
	if hasFile {
		s.disk, s.state, err = readBackend(path, kind)
	} else {
		if initialize == nil {
			return nil, fmt.Errorf("state does not exist")
		}
		s.state, err = initialize()
		if err == nil {
			err = Validate(&s.state)
		}
		if err == nil {
			err = publishState(dir, kind, s.state)
		}
		if err == nil {
			s.disk, s.state, err = readBackend(path, kind)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = Validate(&s.state); err != nil {
		return nil, err
	}
	success = true
	return s, nil
}
func readBackend(path, kind string) (backend, State, error) {
	if kind == JSON {
		s, err := readLegacy(path)
		return &jsonBackend{path: path}, s, err
	}
	return openSQLite(path)
}
func (s *Store) Snapshot() State { s.mu.RLock(); defer s.mu.RUnlock(); return Clone(s.state) }
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("state store is closed")
	}
	next := Clone(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	if err := Validate(&next); err != nil {
		return err
	}
	if err := s.disk.save(next); err != nil {
		return err
	}
	s.state = Clone(next)
	return nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.disk.close(), s.lock.Close())
}
