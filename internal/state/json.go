package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxSnapshotBytes int64 = 64 << 20

func decodeStrict(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document")
	}
	return nil
}
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxSnapshotBytes {
		return nil, fmt.Errorf("state snapshot exceeds 64 MiB")
	}
	return b, nil
}
func readLegacy(path string) (State, error) {
	var s State
	b, err := readLimited(path)
	if err == nil {
		err = decodeStrict(b, &s)
	}
	if err == nil {
		err = Validate(&s)
	}
	if err != nil {
		return State{}, fmt.Errorf("read JSON state: %w", err)
	}
	return s, nil
}

type jsonBackend struct{ path string }

func (j *jsonBackend) close() error { return nil }
func (j *jsonBackend) save(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(j.path, b)
}
func writeTemp(dir string, data []byte) (string, error) {
	if int64(len(data)) > maxSnapshotBytes {
		return "", fmt.Errorf("state snapshot exceeds 64 MiB")
	}
	f, err := os.CreateTemp(dir, ".tunnel-state-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
func replaceFile(path string, data []byte) error {
	name, err := writeTemp(filepath.Dir(path), data)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	return os.Rename(name, path)
}

// linkNew publishes only complete, synced files and never replaces a target.
// A hard link keeps this no-clobber operation atomic on supported local filesystems.
func linkNew(source, target string) error {
	if err := os.Link(source, target); err != nil {
		return fmt.Errorf("publish %s without overwrite: %w", filepath.Base(target), err)
	}
	return nil
}
func publishState(dir, kind string, s State) error {
	filename, err := Filename(kind)
	if err != nil {
		return err
	}
	var temp string
	if kind == JSON {
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		temp, err = writeTemp(dir, raw)
		if err != nil {
			return err
		}
	} else {
		f, err := os.CreateTemp(dir, ".tunnel-sqlite-*")
		if err != nil {
			return err
		}
		temp = f.Name()
		if err = f.Close(); err != nil {
			_ = os.Remove(temp)
			return err
		}
		defer os.Remove(temp)
		db, err := createSQLite(temp, s)
		if err != nil {
			return err
		}
		if err = db.close(); err != nil {
			return err
		}
		f, err = os.OpenFile(temp, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	defer os.Remove(temp)
	return linkNew(temp, filepath.Join(dir, filename))
}
