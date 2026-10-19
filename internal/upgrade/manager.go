package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gofrs/flock"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Record struct {
	HighWater        uint64 `json:"highWater"`
	CurrentSHA       string `json:"currentSHA"`
	CurrentVersion   string `json:"currentVersion"`
	PreviousSHA      string `json:"previousSHA,omitempty"`
	PreviousVersion  string `json:"previousVersion,omitempty"`
	Pending          bool   `json:"pending"`
	CandidateSHA     string `json:"candidateSHA,omitempty"`
	CandidateVersion string `json:"candidateVersion,omitempty"`
}
type Release struct{ manifest Manifest }

func (r *Release) Version() string  { return r.manifest.Version }
func (r *Release) Sequence() uint64 { return r.manifest.Sequence }

type Manager struct {
	dir    string
	key    ed25519.PublicKey
	lock   *flock.Flock
	record Record
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "current.exe"
	}
	return "current"
}
func (m *Manager) Current() string { return filepath.Join(m.dir, executableName()) }
func (m *Manager) Status() Record  { return m.record }
func (m *Manager) Close() error    { return m.lock.Close() }
func regular(path string) error {
	info, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("update path must be a regular file")
	}
	return nil
}
func hashFile(path string) (string, error) {
	if e := regular(path); e != nil {
		return "", e
	}
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, MaxBinarySize+1))
	if e != nil {
		return "", e
	}
	if n == 0 || n > MaxBinarySize {
		return "", fmt.Errorf("invalid executable size")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func atomicBytes(path string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".update-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, path)
	}
	if e == nil {
		e = syncDir(filepath.Dir(path))
	}
	return e
}
func copyFile(from, to string) error {
	if e := regular(from); e != nil {
		return e
	}
	src, e := os.Open(from)
	if e != nil {
		return e
	}
	defer src.Close()
	f, e := os.CreateTemp(filepath.Dir(to), ".binary-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0700); e == nil {
		var n int64
		n, e = io.Copy(f, io.LimitReader(src, MaxBinarySize+1))
		if n == 0 || n > MaxBinarySize {
			e = fmt.Errorf("invalid executable size")
		}
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, to)
	}
	if e == nil {
		e = syncDir(filepath.Dir(to))
	}
	return e
}
func (m *Manager) save() error {
	b, e := json.MarshalIndent(m.record, "", "  ")
	if e != nil {
		return e
	}
	return atomicBytes(filepath.Join(m.dir, "journal.json"), b, 0600)
}
func Open(dir, keyFile, bootstrap string) (*Manager, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("update directory must be a real directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("update directory must be private (0700)")
	}
	for _, name := range []string{executableName(), "previous", "candidate", "journal.json", ".update.lock"} {
		if e := regular(filepath.Join(dir, name)); e != nil && !os.IsNotExist(e) {
			return nil, e
		}
	}
	lock := flock.New(filepath.Join(dir, ".update.lock"))
	ok, e := lock.TryLock()
	if e != nil || !ok {
		lock.Close()
		return nil, fmt.Errorf("update directory is in use")
	}
	m := &Manager{dir: dir, lock: lock}
	success := false
	defer func() {
		if !success {
			lock.Close()
		}
	}()
	if keyFile != "" {
		raw, e := os.ReadFile(keyFile)
		if e != nil {
			return nil, e
		}
		key, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if e != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("public key must be base64 Ed25519 (32 bytes)")
		}
		m.key = ed25519.PublicKey(key)
	}
	journal := filepath.Join(dir, "journal.json")
	raw, e := os.ReadFile(journal)
	if os.IsNotExist(e) {
		if bootstrap == "" {
			return nil, fmt.Errorf("bootstrap binary required for new update directory")
		}
		if _, e := os.Lstat(m.Current()); !os.IsNotExist(e) {
			return nil, fmt.Errorf("untracked current binary; inspect update directory")
		}
		if e = copyFile(bootstrap, m.Current()); e != nil {
			return nil, e
		}
		h, e := hashFile(m.Current())
		if e != nil {
			return nil, e
		}
		m.record = Record{CurrentSHA: h, CurrentVersion: "bootstrap"}
		if e = m.save(); e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	} else {
		if len(raw) > 16384 {
			return nil, fmt.Errorf("update journal exceeds limit")
		}
		if e = strictJSON(raw, &m.record); e != nil {
			return nil, e
		}
		if e = m.Recover(); e != nil {
			return nil, e
		}
	}
	if e = m.verifyCurrent(); e != nil {
		return nil, e
	}
	success = true
	return m, nil
}
func (m *Manager) verifyCurrent() error {
	h, e := hashFile(m.Current())
	if e != nil || h != m.record.CurrentSHA {
		return fmt.Errorf("current executable does not match trusted journal")
	}
	return nil
}
func (m *Manager) Check(ctx context.Context, url string) (*Release, error) {
	if len(m.key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pinned release public key required")
	}
	manifest, e := Fetch(ctx, url, m.key)
	if e != nil {
		return nil, e
	}
	if manifest.Sequence <= m.record.HighWater {
		return nil, nil
	}
	return &Release{manifest: manifest}, nil
}
func (m *Manager) Stage(ctx context.Context, release *Release) error {
	if release == nil {
		return fmt.Errorf("verified release required")
	}
	manifest := release.manifest
	// Check signature at the fetch boundary; callers cannot stage arbitrary values.
	if e := validateManifest(manifest, time.Now()); e != nil {
		return e
	}
	if manifest.Sequence <= m.record.HighWater {
		return fmt.Errorf("update sequence is not newer")
	}
	var selected *Artifact
	for i := range manifest.Artifacts {
		if manifest.Artifacts[i].Platform == runtime.GOOS+"/"+runtime.GOARCH {
			selected = &manifest.Artifacts[i]
		}
	}
	if selected == nil {
		return fmt.Errorf("manifest has no executable for this platform")
	}
	res, e := download(ctx, selected.URL)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	f, e := os.CreateTemp(m.dir, ".download-*")
	if e != nil {
		return e
	}
	path := f.Name()
	defer os.Remove(path)
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, selected.Size+1))
	if e == nil && (n != selected.Size || hex.EncodeToString(h.Sum(nil)) != selected.SHA256) {
		e = fmt.Errorf("download size or SHA-256 mismatch")
	}
	if e == nil {
		e = f.Chmod(0700)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(path, filepath.Join(m.dir, "candidate")); e != nil {
		return e
	}
	return syncDir(m.dir)
}
func (m *Manager) Install(release *Release) error {
	if release == nil {
		return fmt.Errorf("verified release required")
	}
	manifest := release.manifest
	if err := validateManifest(manifest, time.Now()); err != nil {
		return err
	}
	var artifact *Artifact
	for i := range manifest.Artifacts {
		a := &manifest.Artifacts[i]
		if a.Platform == runtime.GOOS+"/"+runtime.GOARCH {
			artifact = a
		}
	}
	if artifact == nil || manifest.Sequence <= m.record.HighWater {
		return fmt.Errorf("no staged newer platform artifact")
	}
	h, e := hashFile(filepath.Join(m.dir, "candidate"))
	if e != nil || h != artifact.SHA256 {
		return fmt.Errorf("staged binary verification failed")
	}
	return m.begin(artifact.SHA256, manifest.Version, manifest.Sequence)
}
func (m *Manager) begin(hash, version string, sequence uint64) error {
	if e := m.verifyCurrent(); e != nil {
		return e
	}
	m.record.Pending = true
	m.record.CandidateSHA = hash
	m.record.CandidateVersion = version
	if sequence > m.record.HighWater {
		m.record.HighWater = sequence
	}
	if e := m.save(); e != nil {
		return e
	}
	if e := copyFile(m.Current(), filepath.Join(m.dir, "previous")); e != nil {
		return e
	}
	if e := os.Rename(filepath.Join(m.dir, "candidate"), m.Current()); e != nil {
		return e
	}
	return syncDir(m.dir)
}
func (m *Manager) Commit() error {
	if !m.record.Pending {
		return fmt.Errorf("no pending update")
	}
	h, e := hashFile(m.Current())
	if e != nil || h != m.record.CandidateSHA {
		return fmt.Errorf("candidate changed before readiness commit")
	}
	m.record.PreviousSHA = m.record.CurrentSHA
	m.record.PreviousVersion = m.record.CurrentVersion
	m.record.CurrentSHA = m.record.CandidateSHA
	m.record.CurrentVersion = m.record.CandidateVersion
	m.record.CandidateSHA = ""
	m.record.CandidateVersion = ""
	m.record.Pending = false
	return m.save()
}
func (m *Manager) Recover() error {
	if !m.record.Pending {
		return m.verifyCurrent()
	}
	h, e := hashFile(m.Current())
	if e != nil || h != m.record.CurrentSHA {
		h, e = hashFile(filepath.Join(m.dir, "previous"))
		if e != nil || h != m.record.CurrentSHA {
			return fmt.Errorf("pending update has no verified recovery binary")
		}
		if e = copyFile(filepath.Join(m.dir, "previous"), m.Current()); e != nil {
			return e
		}
	}
	m.record.PreviousSHA = ""
	m.record.PreviousVersion = ""
	m.record.CandidateSHA = ""
	m.record.CandidateVersion = ""
	m.record.Pending = false
	return m.save()
}
func (m *Manager) Rollback() error {
	if m.record.Pending {
		return m.Recover()
	}
	h, e := hashFile(filepath.Join(m.dir, "previous"))
	if e != nil || h != m.record.PreviousSHA || h == "" {
		return fmt.Errorf("verified previous executable unavailable")
	}
	version := m.record.PreviousVersion
	if e = copyFile(filepath.Join(m.dir, "previous"), filepath.Join(m.dir, "candidate")); e != nil {
		return e
	}
	if e = m.begin(h, version, 0); e != nil {
		return e
	}
	return m.Commit()
}
