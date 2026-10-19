package state

import (
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"strings"
	"testing"
	"time"
)

func BenchmarkSQLiteSparseRename(b *testing.B) {
	b.StopTimer()
	hash, e := bcrypt.GenerateFromPassword([]byte("benchmark-password"), bcrypt.MinCost)
	if e != nil {
		b.Fatal(e)
	}
	s := State{Users: []User{{ID: "root", Username: "root", Role: "admin", PasswordHash: string(hash), SiteIDs: []string{}, ServiceIDs: []string{}}}, Sites: []Site{{ID: "site", Name: "Site", TokenHash: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}}, Services: []Service{{ID: "app", Name: "App", SiteID: "site", URL: "http://127.0.0.1:3000"}}}
	for i := 0; i < 1000; i++ {
		s.Users = append(s.Users, User{ID: fmt.Sprintf("user-%d", i), Username: fmt.Sprintf("user-%d", i), Role: "viewer", PasswordHash: string(hash), SiteIDs: []string{"site"}, ServiceIDs: []string{}})
	}
	r, e := Open(b.TempDir(), SQLite, func() (State, error) { return s, nil })
	if e != nil {
		b.Fatal(e)
	}
	defer r.Close()
	db := r.disk.(*sqliteBackend).db
	var before, after int64
	if e = db.QueryRow("SELECT total_changes()").Scan(&before); e != nil {
		b.Fatal(e)
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if e = r.Update(func(next *State) error { next.Sites[0].Name = fmt.Sprintf("Site %d", i); return nil }); e != nil {
			b.Fatal(e)
		}
	}
	b.StopTimer()
	if e = db.QueryRow("SELECT total_changes()").Scan(&after); e != nil {
		b.Fatal(e)
	}
	b.ReportMetric(float64(after-before)/float64(b.N), "sql_rows/op")
}
