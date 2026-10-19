package state

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func agentSample(t *testing.T) State {
	s := sample(t)
	s.Sites[0].LegacyDisabled = true
	s.Agents = []AgentRegistration{{ID: "agent-a", SiteID: s.Sites[0].ID, Name: "Edge A", TokenHash: strings.Repeat("b", 64), CreatedAt: s.Sites[0].CreatedAt}}
	return s
}
func TestAgentsPersistenceBackupAndV6Upgrade(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			s := agentSample(t)
			dir := t.TempDir()
			r := mustOpen(t, dir, kind, s)
			r.Close()
			r = mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(s, r.Snapshot()) {
				t.Fatal("Agent data lost")
			}
			r.Close()
			path := filepath.Join(t.TempDir(), "backup.json")
			if _, e := Export(dir, kind, path); e != nil {
				t.Fatal(e)
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, e := Restore(dest, target, path); e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(s, mustOpen(t, dest, target, State{}).Snapshot()) {
					t.Fatal("Agent backup changed")
				}
			}
		})
	}
	dir := t.TempDir()
	s := sample(t)
	r := mustOpen(t, dir, SQLite, s)
	for _, q := range []string{"DROP TABLE agents", "ALTER TABLE sites DROP COLUMN legacy_disabled", "PRAGMA user_version=6"} {
		if _, e := r.disk.(*sqliteBackend).db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	r.Close()
	if !reflect.DeepEqual(s, mustOpen(t, dir, SQLite, State{}).Snapshot()) {
		t.Fatal("v6 upgrade changed legacy data")
	}
}
func TestAgentsRejectInvalidReferencesAndCredentials(t *testing.T) {
	for _, mode := range []string{"orphan", "reserved", "duplicate", "site-token", "hash", "rotation", "limit"} {
		t.Run(mode, func(t *testing.T) {
			s := agentSample(t)
			switch mode {
			case "orphan":
				s.Agents[0].SiteID = "unknown"
			case "reserved":
				s.Agents[0].ID = "legacy"
			case "duplicate":
				s.Agents = append(s.Agents, s.Agents[0])
			case "site-token":
				s.Agents[0].TokenHash = s.Sites[0].TokenHash
			case "hash":
				s.Agents[0].TokenHash = "bad"
			case "rotation":
				s.Agents[0].RequiresRotation = true
			case "limit":
				for i := 0; i < 17; i++ {
					a := s.Agents[0]
					a.ID = "instance-" + strings.Repeat("a", i+1)
					a.TokenHash = fmt.Sprintf("%064x", i+256)
					s.Agents = append(s.Agents, a)
				}
			}
			if Validate(&s) == nil {
				t.Fatal("invalid registration accepted")
			}
		})
	}
}
