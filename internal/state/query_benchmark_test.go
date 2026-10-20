package state

import (
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"strings"
	"testing"
	"time"
)

func BenchmarkStorageQueries(b *testing.B) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("benchmark-password"), bcrypt.MinCost)
	st := State{Users: []User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash)}}, Sites: []Site{{ID: "site-a", Name: "Site", TokenHash: strings.Repeat("a", 64), CreatedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}}}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("user-%04d", i)
		st.Users = append(st.Users, User{ID: id, Username: id, Role: "viewer", PasswordHash: string(hash)})
		st.Groups = append(st.Groups, Group{ID: fmt.Sprintf("group-%04d", i), Name: id, MemberIDs: []string{id}})
	}
	s, err := Open(b.TempDir(), SQLite, func() (State, error) { return st, nil })
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []string{"login-snapshot", "login-point", "directory-snapshot-100", "directory-sql-100", "membership-json-scan", "membership-index"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch mode {
				case "login-snapshot":
					found := false
					for _, u := range s.Snapshot().Users {
						if u.Username == "user-0999" {
							found = true
							break
						}
					}
					if !found {
						b.Fatal("missing user")
					}
				case "login-point":
					if _, found, err := s.UserByUsername("user-0999"); err != nil || !found {
						b.Fatal(err)
					}
				case "directory-snapshot-100":
					page, _, _ := memoryDirectory(s.Snapshot(), DirectoryQuery{Kind: "users", Filter: DirectoryFilter{Global: true}, Limit: 100}, -1)
					if len(page.Users) != 100 {
						b.Fatal("missing users")
					}
				case "directory-sql-100":
					page, err := s.Directory(DirectoryQuery{Kind: "users", Filter: DirectoryFilter{Global: true}, Limit: 100})
					if err != nil || len(page.State.Users) != 100 {
						b.Fatal(err)
					}
				default:
					sql := `SELECT g.payload FROM groups g WHERE EXISTS (SELECT 1 FROM json_each(g.payload,'$.memberIDs') m WHERE m.value=?)`
					if mode == "membership-index" {
						sql = `SELECT g.payload FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=?`
					}
					rows, err := s.disk.(*sqliteBackend).db.Query(sql, "user-0999")
					if err != nil {
						b.Fatal(err)
					}
					count := 0
					for rows.Next() {
						var raw string
						if err = rows.Scan(&raw); err != nil {
							b.Fatal(err)
						}
						count++
					}
					err = rows.Err()
					rows.Close()
					if err != nil || count != 1 {
						b.Fatal("missing group", err)
					}
				}
			}
		})
	}
}
