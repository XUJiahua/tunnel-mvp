package state

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxOperationsBytes int64 = 2 << 30
const operationsPending = "operations.restore.pending"

var operationsName = regexp.MustCompile(`^(audit\.jsonl(\.([1-9]|1[0-9]|20))?|monitor\.db)$`)

type OperationFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type OperationsManifest struct {
	Format    string          `json:"format"`
	Version   int             `json:"version"`
	CreatedAt time.Time       `json:"createdAt"`
	Files     []OperationFile `json:"files"`
}
type OperationsSummary struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

func operationSummary(m OperationsManifest) OperationsSummary {
	out := OperationsSummary{Files: len(m.Files)}
	for _, f := range m.Files {
		out.Bytes += f.Size
	}
	return out
}
func CheckOperationsRestore(dir string) error {
	if found, err := exists(filepath.Join(dir, operationsPending)); err != nil {
		return err
	} else if found {
		return fmt.Errorf("operational restore is incomplete; resume restore-operations with the same archive before starting Server")
	}
	return nil
}
func operationDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxOperationsBytes+1))
	if err != nil {
		return "", 0, err
	}
	if n > maxOperationsBytes {
		return "", 0, fmt.Errorf("operations archive exceeds 2 GiB")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func validateMonitorArchive(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	normalized := filepath.ToSlash(abs)
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: normalized, RawQuery: "mode=ro"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	var app, version int
	var integrity string
	if err = db.QueryRow("PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if app != 0x544d4f4e || version < 1 || version > 3 {
		return fmt.Errorf("unsupported monitoring archive database")
	}
	if err = db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("monitoring archive integrity check failed")
	}
	queries := []string{"SELECT service_id,profile,checked_at,reachable,healthy,status,latency_ms,request_id,code,stage FROM samples LIMIT 0"}
	if version >= 2 {
		queries = append(queries, "SELECT organization_id,occurred_at,event,username,site_id,resource_id,payload FROM audit_events LIMIT 0", "SELECT epoch,file_count,skipped FROM audit_index_state WHERE singleton=1")
	}
	if version >= 3 {
		queries = append(queries, "SELECT source_id FROM audit_events LIMIT 0", "SELECT id,slot,skipped FROM audit_sources LIMIT 0")
	}
	for _, q := range queries {
		rows, e := db.Query(q)
		if e != nil {
			return fmt.Errorf("invalid monitoring archive schema: %w", e)
		}
		rows.Close()
	}
	rows, e := db.Query("PRAGMA foreign_key_check")
	if e != nil {
		return e
	}
	invalid := rows.Next()
	rowErr := rows.Err()
	rows.Close()
	if invalid || rowErr != nil {
		return fmt.Errorf("monitoring archive has invalid foreign keys")
	}
	if version >= 3 {
		var invalidSources int
		if e = db.QueryRow("SELECT count(*) FROM audit_sources WHERE id<1 OR slot<0 OR slot>20 OR skipped<0").Scan(&invalidSources); e != nil || invalidSources != 0 {
			return fmt.Errorf("monitoring archive contains invalid audit sources")
		}
	}
	return validateObservationArchive(db)
}
func ExportOperations(dir, output string) (OperationsSummary, error) {
	lock, err := lockExistingDir(dir)
	if err != nil {
		return OperationsSummary{}, err
	}
	defer lock.Close()
	if err = CheckOperationsRestore(dir); err != nil {
		return OperationsSummary{}, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if found, e := exists(filepath.Join(dir, "monitor.db"+suffix)); e != nil {
			return OperationsSummary{}, e
		} else if found {
			return OperationsSummary{}, fmt.Errorf("monitor database sidecar exists; recover with Server, then stop before archiving")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return OperationsSummary{}, err
	}
	m := OperationsManifest{Format: "tunnel-operations", Version: 1, CreatedAt: time.Now().UTC(), Files: []OperationFile{}}
	for _, entry := range entries {
		if !operationsName.MatchString(entry.Name()) {
			continue
		}
		name := filepath.Join(dir, entry.Name())
		info, e := os.Lstat(name)
		if e != nil {
			return OperationsSummary{}, e
		}
		if !info.Mode().IsRegular() {
			return OperationsSummary{}, fmt.Errorf("operational files must be regular, not symbolic links")
		}
		sum, size, e := operationDigest(name)
		if e != nil {
			return OperationsSummary{}, e
		}
		m.Files = append(m.Files, OperationFile{entry.Name(), size, sum})
	}
	if len(m.Files) == 0 {
		return OperationsSummary{}, fmt.Errorf("no audit or monitoring files found")
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	summary := operationSummary(m)
	if summary.Bytes > maxOperationsBytes {
		return OperationsSummary{}, fmt.Errorf("operations archive exceeds 2 GiB")
	}
	for _, f := range m.Files {
		if f.Name == "monitor.db" {
			if err = validateMonitorArchive(filepath.Join(dir, f.Name)); err != nil {
				return OperationsSummary{}, err
			}
		}
	}
	f, err := os.CreateTemp(filepath.Dir(output), ".tunnel-operations-*")
	if err != nil {
		return OperationsSummary{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	raw, _ := json.Marshal(m)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(raw)), ModTime: m.CreatedAt}); err != nil {
		return OperationsSummary{}, err
	}
	if _, err = tw.Write(raw); err != nil {
		return OperationsSummary{}, err
	}
	for _, item := range m.Files {
		if err = tw.WriteHeader(&tar.Header{Name: item.Name, Mode: 0600, Size: item.Size, ModTime: m.CreatedAt}); err != nil {
			return OperationsSummary{}, err
		}
		source, e := os.Open(filepath.Join(dir, item.Name))
		if e != nil {
			return OperationsSummary{}, e
		}
		_, e = io.Copy(tw, source)
		source.Close()
		if e != nil {
			return OperationsSummary{}, e
		}
	}
	if err = tw.Close(); err != nil {
		return OperationsSummary{}, err
	}
	if err = gz.Close(); err != nil {
		return OperationsSummary{}, err
	}
	if err = f.Sync(); err != nil {
		return OperationsSummary{}, err
	}
	if err = f.Close(); err != nil {
		return OperationsSummary{}, err
	}
	if _, err = ValidateOperations(name); err != nil {
		return OperationsSummary{}, err
	}
	if err = linkNew(name, output); err != nil {
		return OperationsSummary{}, err
	}
	return summary, nil
}
func readOperations(input string) (OperationsManifest, string, error) {
	var m OperationsManifest
	f, err := os.Open(input)
	if err != nil {
		return m, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > maxOperationsBytes {
		return m, "", fmt.Errorf("invalid operations archive size")
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return m, "", err
	}
	defer gz.Close()
	stage, err := os.MkdirTemp("", "tunnel-operations-verify-")
	if err != nil {
		return m, "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(stage)
		}
	}()
	tr := tar.NewReader(gz)
	files := map[string]OperationFile{}
	seen := map[string]bool{}
	total := int64(0)
	for {
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return m, "", e
		}
		if (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || seen[header.Name] || header.Size < 0 || (!operationsName.MatchString(header.Name) && header.Name != "manifest.json") {
			return m, "", fmt.Errorf("invalid or duplicate operational archive entry")
		}
		seen[header.Name] = true
		if header.Name == "manifest.json" {
			if header.Size > 256<<10 {
				return m, "", fmt.Errorf("operations manifest too large")
			}
			raw, e := io.ReadAll(io.LimitReader(tr, header.Size))
			if e != nil {
				return m, "", e
			}
			if e = decodeStrict(raw, &m); e != nil {
				return m, "", e
			}
			continue
		}
		total += header.Size
		if total > maxOperationsBytes {
			return m, "", fmt.Errorf("operations archive exceeds 2 GiB")
		}
		target, e := os.OpenFile(filepath.Join(stage, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return m, "", e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(target, hash), tr)
		closeErr := target.Close()
		if e != nil {
			return m, "", e
		}
		if closeErr != nil {
			return m, "", closeErr
		}
		if n != header.Size {
			return m, "", fmt.Errorf("truncated operational file")
		}
		files[header.Name] = OperationFile{header.Name, n, hex.EncodeToString(hash.Sum(nil))}
	}
	// Tar EOF alone does not validate the gzip footer; consume it and reject
	// nonzero appended streams or unbounded trailing padding.
	padding, err := io.ReadAll(io.LimitReader(gz, 1<<20))
	if err != nil {
		return m, "", err
	}
	if len(padding) >= 1<<20 || len(strings.Trim(string(padding), "\x00")) != 0 {
		return m, "", fmt.Errorf("unexpected trailing operations data")
	}
	if m.Format != "tunnel-operations" || m.Version != 1 || m.CreatedAt.IsZero() || len(m.Files) == 0 || len(m.Files) != len(files) {
		return m, "", fmt.Errorf("invalid operations manifest")
	}
	manifestNames := map[string]bool{}
	for _, item := range m.Files {
		if manifestNames[item.Name] || !operationsName.MatchString(item.Name) || files[item.Name] != item {
			return m, "", fmt.Errorf("operations checksum/size mismatch")
		}
		manifestNames[item.Name] = true
		if item.Name == "monitor.db" {
			if err = validateMonitorArchive(filepath.Join(stage, item.Name)); err != nil {
				return m, "", err
			}
		}
	}
	ok = true
	return m, stage, nil
}
func ValidateOperations(input string) (OperationsSummary, error) {
	m, stage, err := readOperations(input)
	if err != nil {
		return OperationsSummary{}, err
	}
	defer os.RemoveAll(stage)
	return operationSummary(m), nil
}
func RestoreOperations(dir, input string) (OperationsSummary, error) {
	m, stage, err := readOperations(input)
	if err != nil {
		return OperationsSummary{}, err
	}
	defer os.RemoveAll(stage)
	lock, err := lockDir(dir)
	if err != nil {
		return OperationsSummary{}, err
	}
	defer lock.Close()
	marker := filepath.Join(dir, operationsPending)
	raw, _ := json.Marshal(m)
	pending, err := os.ReadFile(marker)
	resume := err == nil
	if err != nil && !os.IsNotExist(err) {
		return OperationsSummary{}, err
	}
	if resume {
		var previous OperationsManifest
		if decodeStrict(pending, &previous) != nil {
			return OperationsSummary{}, fmt.Errorf("invalid pending operational restore marker")
		}
		previousRaw, _ := json.Marshal(previous)
		if string(previousRaw) != string(raw) {
			return OperationsSummary{}, fmt.Errorf("pending restore requires the same operations archive")
		}
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if found, e := exists(filepath.Join(dir, "monitor.db"+suffix)); e != nil {
			return OperationsSummary{}, e
		} else if found {
			return OperationsSummary{}, fmt.Errorf("target contains monitor database sidecar")
		}
	}
	if !resume {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return OperationsSummary{}, e
		}
		for _, entry := range entries {
			if operationsName.MatchString(entry.Name()) {
				return OperationsSummary{}, fmt.Errorf("restore requires a directory without operational files")
			}
		}
		for _, item := range m.Files {
			if found, e := exists(filepath.Join(dir, item.Name)); e != nil {
				return OperationsSummary{}, e
			} else if found {
				return OperationsSummary{}, fmt.Errorf("target already contains operational file %s", item.Name)
			}
		}
		f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return OperationsSummary{}, e
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return OperationsSummary{}, e
		}
		if closeErr != nil {
			return OperationsSummary{}, closeErr
		}
	}
	for _, item := range m.Files {
		target := filepath.Join(dir, item.Name)
		if found, e := exists(target); e != nil {
			return OperationsSummary{}, e
		} else if found {
			info, e := os.Lstat(target)
			if e != nil || !info.Mode().IsRegular() {
				return OperationsSummary{}, fmt.Errorf("restore target is not a regular file")
			}
			sum, size, e := operationDigest(target)
			if e != nil || sum != item.SHA256 || size != item.Size {
				return OperationsSummary{}, fmt.Errorf("pending restore target checksum mismatch")
			}
			continue
		}
		source, e := os.Open(filepath.Join(stage, item.Name))
		if e != nil {
			return OperationsSummary{}, e
		}
		temp, e := os.CreateTemp(dir, ".tunnel-operations-restore-*")
		if e != nil {
			source.Close()
			return OperationsSummary{}, e
		}
		name := temp.Name()
		_, e = io.Copy(temp, source)
		source.Close()
		if e == nil {
			e = temp.Sync()
		}
		closeErr := temp.Close()
		if e == nil {
			e = closeErr
		}
		if e == nil {
			e = linkNew(name, target)
		}
		os.Remove(name)
		if e != nil {
			return OperationsSummary{}, e
		}
	}
	if err = os.Remove(marker); err != nil {
		return OperationsSummary{}, err
	}
	return operationSummary(m), nil
}

// Extension validation remains additive: HA-3 archives without these tables are
// accepted; a present extension must be complete and internally consistent.
func validateObservationArchive(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'observation_%'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	var version int
	var source, node string
	var buffered, errors int64
	if err := db.QueryRow(`SELECT version,source,node_id,buffered_bytes,enqueue_errors FROM observation_local WHERE singleton=1`).Scan(&version, &source, &node, &buffered, &errors); err != nil {
		return fmt.Errorf("invalid observation archive metadata: %w", err)
	}
	if version != ObservationVersion || !observationHex(source, 24) || !ValidClusterNodeID(node) || buffered < 0 || errors < 0 {
		return ErrObservationSchema
	}
	var bytes int64
	if err := db.QueryRow(`SELECT coalesce(sum(length(CAST(record AS BLOB))),0) FROM observation_queue`).Scan(&bytes); err != nil {
		return err
	}
	if bytes != buffered {
		return fmt.Errorf("observation archive byte counter mismatch")
	}
	rows, err := db.Query(`SELECT event_id,record FROM observation_queue ORDER BY seq`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var o Observation
		if err = json.Unmarshal(raw, &o); err != nil {
			break
		}
		if !validObservation(o) || o.ID != id || o.Source != source || o.NodeID != node {
			err = fmt.Errorf("invalid observation archive queue identity")
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, q := range []string{`SELECT event_id,epoch FROM observation_sample_identity LIMIT 0`, `SELECT event_id FROM observation_audit_identity LIMIT 0`} {
		rows, err := db.Query(q)
		if err != nil {
			return err
		}
		rows.Close()
	}
	return nil
}
