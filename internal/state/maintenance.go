package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Summary struct {
	Storage       string `json:"storage,omitempty"`
	SchemaVersion int    `json:"schemaVersion"`
	Users         int    `json:"users"`
	Sites         int    `json:"sites"`
	Services      int    `json:"services"`
}

func summarize(kind string, s State) Summary {
	return Summary{kind, SchemaVersion, len(s.Users), len(s.Sites), len(s.Services)}
}

type Backup struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	SHA256    string    `json:"sha256"`
	State     State     `json:"state"`
}

func checksum(s State) string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func loadBackup(path string) (State, error) {
	raw, err := readLimited(path)
	if err != nil {
		return State{}, err
	}
	var b Backup
	if err = decodeStrict(raw, &b); err != nil {
		return State{}, fmt.Errorf("read backup: %w", err)
	}
	if b.Format != "tunnel-state" || (b.Version < 1 || b.Version > 4) || b.CreatedAt.IsZero() {
		return State{}, fmt.Errorf("unsupported backup format/version")
	}
	if b.SHA256 != checksum(b.State) {
		return State{}, fmt.Errorf("backup checksum mismatch")
	}
	if err = Validate(&b.State); err != nil {
		return State{}, err
	}
	return b.State, nil
}
func ValidateBackup(path string) (Summary, error) {
	s, err := loadBackup(path)
	if err != nil {
		return Summary{}, err
	}
	return summarize("", s), nil
}
func existing(dir, kind string) (backend, State, error) {
	name, err := Filename(kind)
	if err != nil {
		return nil, State{}, err
	}
	return readBackend(filepath.Join(dir, name), kind)
}
func Inspect(dir, kind string) (Summary, error) {
	lock, err := lockExistingDir(dir)
	if err != nil {
		return Summary{}, err
	}
	defer lock.Close()
	b, s, err := existing(dir, kind)
	if err != nil {
		return Summary{}, err
	}
	defer b.close()
	return summarize(kind, s), nil
}
func Export(dir, kind, output string) (Summary, error) {
	lock, err := lockExistingDir(dir)
	if err != nil {
		return Summary{}, err
	}
	defer lock.Close()
	b, s, err := existing(dir, kind)
	if err != nil {
		return Summary{}, err
	}
	defer b.close()
	raw, err := json.MarshalIndent(Backup{"tunnel-state", 4, time.Now().UTC(), checksum(s), s}, "", "  ")
	if err != nil {
		return Summary{}, err
	}
	temp, err := writeTemp(filepath.Dir(output), raw)
	if err != nil {
		return Summary{}, err
	}
	defer os.Remove(temp)
	if err = linkNew(temp, output); err != nil {
		return Summary{}, err
	}
	return summarize(kind, s), nil
}
func MigrateJSON(dir string) (Summary, error) {
	lock, err := lockExistingDir(dir)
	if err != nil {
		return Summary{}, err
	}
	defer lock.Close()
	if found, err := exists(filepath.Join(dir, "state.db")); err != nil {
		return Summary{}, err
	} else if found {
		return Summary{}, fmt.Errorf("state.db already exists; refusing to overwrite")
	}
	if err := rejectSidecars(dir); err != nil {
		return Summary{}, err
	}
	s, err := readLegacy(filepath.Join(dir, "state.json"))
	if err != nil {
		return Summary{}, err
	}
	if err = publishState(dir, SQLite, s); err != nil {
		return Summary{}, err
	}
	return summarize(SQLite, s), nil
}
func Restore(dir, kind, input string) (Summary, error) {
	// Validate before creating any target directory or taking its lock.
	s, err := loadBackup(input)
	if err != nil {
		return Summary{}, err
	}
	// Restoring a historical backup must not resurrect a consumed invite token.
	s.Invites = nil
	if _, err = Filename(kind); err != nil {
		return Summary{}, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return Summary{}, err
	}
	defer lock.Close()
	for _, name := range []string{"state.json", "state.db"} {
		if found, err := exists(filepath.Join(dir, name)); err != nil {
			return Summary{}, err
		} else if found {
			return Summary{}, fmt.Errorf("target contains %s; restore into a fresh data directory", name)
		}
	}
	if err = rejectSidecars(dir); err != nil {
		return Summary{}, err
	}
	if err = publishState(dir, kind, s); err != nil {
		return Summary{}, err
	}
	return summarize(kind, s), nil
}

// ResetAdminPassword is an explicit offline recovery operation. It never creates
// a user or bypasses state validation, and shares the Server's lifetime lock.
func ResetAdminPassword(dir, kind, username, password string) (Summary, error) {
	if len(password) < 12 || len(password) > 72 {
		return Summary{}, fmt.Errorf("recovery password must be 12–72 bytes")
	}
	if username == "" {
		return Summary{}, fmt.Errorf("administrator username required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Summary{}, err
	}
	repo, err := Open(dir, kind, nil)
	if err != nil {
		return Summary{}, err
	}
	defer repo.Close()
	err = repo.Update(func(next *State) error {
		for i := range next.Users {
			if next.Users[i].Username == username && next.Users[i].Role == "admin" {
				next.Users[i].PasswordHash = string(hash)
				next.Users[i].Disabled = false
				return nil
			}
		}
		return fmt.Errorf("existing administrator not found")
	})
	if err != nil {
		return Summary{}, err
	}
	return summarize(kind, repo.Snapshot()), nil
}
