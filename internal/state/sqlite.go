package state

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 1
const applicationID = 0x54554e4c // TUNL

type sqliteBackend struct{ db *sql.DB }

func connectSQLite(path string) (*sqliteBackend, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	normalized := filepath.ToSlash(absolute)
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	query := url.Values{"mode": {"rw"}, "_pragma": {"foreign_keys(1)", "busy_timeout(5000)", "synchronous(FULL)"}}
	dsn := (&url.URL{Scheme: "file", Path: normalized, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	return &sqliteBackend{db}, nil
}

var schema = []string{
	`CREATE TABLE users (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','viewer')))`,
	`CREATE TABLE sites (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, token_hash TEXT NOT NULL CHECK(length(token_hash)=64), created_at TEXT NOT NULL)`,
	`CREATE TABLE services (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, site_id TEXT NOT NULL REFERENCES sites(id), url TEXT NOT NULL)`,
	`CREATE TABLE user_sites (user_id TEXT NOT NULL REFERENCES users(id), site_id TEXT NOT NULL REFERENCES sites(id), position INTEGER NOT NULL, PRIMARY KEY(user_id,site_id), UNIQUE(user_id,position))`,
	`CREATE TABLE user_services (user_id TEXT NOT NULL REFERENCES users(id), service_id TEXT NOT NULL REFERENCES services(id), position INTEGER NOT NULL, PRIMARY KEY(user_id,service_id), UNIQUE(user_id,position))`,
	`CREATE INDEX services_site ON services(site_id)`,
}

func createSQLite(path string, s State) (*sqliteBackend, error) {
	b, err := connectSQLite(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = b.close()
		}
	}()
	if _, err = b.db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		return nil, err
	}
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, statement := range schema {
		if _, err = tx.Exec(statement); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA application_id=%d", applicationID)); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = b.save(s); err != nil {
		return nil, err
	}
	ok = true
	return b, nil
}
func openSQLite(path string) (backend, State, error) {
	b, err := connectSQLite(path)
	if err != nil {
		return nil, State{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = b.close()
		}
	}()
	var app, version int
	if err = b.db.QueryRow("PRAGMA application_id").Scan(&app); err != nil {
		return nil, State{}, err
	}
	if err = b.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, State{}, err
	}
	if app != applicationID || version != SchemaVersion {
		return nil, State{}, fmt.Errorf("unsupported SQLite application/schema: app=%d version=%d (expected %d)", app, version, SchemaVersion)
	}
	var integrity string
	if err = b.db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil {
		return nil, State{}, err
	}
	if integrity != "ok" {
		return nil, State{}, fmt.Errorf("SQLite integrity check failed")
	}
	rows, err := b.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return nil, State{}, err
	}
	invalid := rows.Next()
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return nil, State{}, rowErr
	}
	if invalid {
		return nil, State{}, fmt.Errorf("SQLite contains invalid foreign keys")
	}
	s, err := b.load()
	if err != nil {
		return nil, State{}, err
	}
	if err = Validate(&s); err != nil {
		return nil, State{}, err
	}
	ok = true
	return b, s, nil
}
func (b *sqliteBackend) close() error { return b.db.Close() }
func (b *sqliteBackend) save(s State) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// This version deliberately writes one validated aggregate per transaction.
	// The in-memory read model stays fast; incremental SQL updates can be added
	// behind Repository without changing control-plane authorization handlers.
	for _, table := range []string{"user_services", "user_sites", "services", "sites", "users"} {
		if _, err = tx.Exec("DELETE FROM " + table); err != nil {
			return err
		}
	}
	for i, v := range s.Users {
		if _, err = tx.Exec("INSERT INTO users VALUES(?,?,?,?,?)", v.ID, i, v.Username, v.PasswordHash, v.Role); err != nil {
			return err
		}
	}
	for i, v := range s.Sites {
		if _, err = tx.Exec("INSERT INTO sites VALUES(?,?,?,?,?)", v.ID, i, v.Name, v.TokenHash, v.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	for i, v := range s.Services {
		if _, err = tx.Exec("INSERT INTO services VALUES(?,?,?,?,?)", v.ID, i, v.Name, v.SiteID, v.URL); err != nil {
			return err
		}
	}
	for _, v := range s.Users {
		for i, id := range v.SiteIDs {
			if _, err = tx.Exec("INSERT INTO user_sites VALUES(?,?,?)", v.ID, id, i); err != nil {
				return err
			}
		}
		for i, id := range v.ServiceIDs {
			if _, err = tx.Exec("INSERT INTO user_services VALUES(?,?,?)", v.ID, id, i); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (b *sqliteBackend) load() (State, error) {
	s := State{Users: []User{}, Sites: []Site{}, Services: []Service{}}
	rows, err := b.db.Query("SELECT id,username,password_hash,role FROM users ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var u User
		u.SiteIDs = []string{}
		u.ServiceIDs = []string{}
		if err = rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role); err != nil {
			_ = rows.Close()
			return s, err
		}
		s.Users = append(s.Users, u)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = b.db.Query("SELECT id,name,token_hash,created_at FROM sites ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var v Site
		var created string
		if err = rows.Scan(&v.ID, &v.Name, &v.TokenHash, &created); err != nil {
			_ = rows.Close()
			return s, err
		}
		v.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			_ = rows.Close()
			return s, err
		}
		s.Sites = append(s.Sites, v)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = b.db.Query("SELECT id,name,site_id,url FROM services ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var v Service
		if err = rows.Scan(&v.ID, &v.Name, &v.SiteID, &v.URL); err != nil {
			_ = rows.Close()
			return s, err
		}
		s.Services = append(s.Services, v)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return s, err
	}
	index := map[string]int{}
	for i, u := range s.Users {
		index[u.ID] = i
	}
	for _, table := range []string{"user_sites", "user_services"} {
		column := "site_id"
		if table == "user_services" {
			column = "service_id"
		}
		rows, err = b.db.Query("SELECT user_id," + column + " FROM " + table + " ORDER BY user_id,position")
		if err != nil {
			return s, err
		}
		for rows.Next() {
			var user, id string
			if err = rows.Scan(&user, &id); err != nil {
				_ = rows.Close()
				return s, err
			}
			i, ok := index[user]
			if !ok {
				_ = rows.Close()
				return s, fmt.Errorf("unknown grant owner")
			}
			if table == "user_sites" {
				s.Users[i].SiteIDs = append(s.Users[i].SiteIDs, id)
			} else {
				s.Users[i].ServiceIDs = append(s.Users[i].ServiceIDs, id)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return s, err
		}
	}
	return s, nil
}
