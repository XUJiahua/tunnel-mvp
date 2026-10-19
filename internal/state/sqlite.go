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

const SchemaVersion = 10
const applicationID = 0x54554e4c // TUNL

type sqliteBackend struct {
	db     *sql.DB
	rows   []sqlTable
	grants map[string]userGrants
}

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
	return &sqliteBackend{db: db}, nil
}

// A derived non-unique index adds no storage semantics or backup version.
const resourceDomainIndex = `CREATE INDEX IF NOT EXISTS services_custom_domain ON services(custom_domain) WHERE custom_domain != ''`

var schemaV7 = []string{`CREATE TABLE agents (id TEXT PRIMARY KEY,position INTEGER NOT NULL UNIQUE,payload TEXT NOT NULL CHECK(json_valid(payload)))`}
var schemaV4 = []string{
	`CREATE TABLE organizations (id TEXT PRIMARY KEY,position INTEGER NOT NULL UNIQUE,payload TEXT NOT NULL CHECK(json_valid(payload)))`,
	`CREATE TABLE roles (id TEXT PRIMARY KEY,position INTEGER NOT NULL UNIQUE,payload TEXT NOT NULL CHECK(json_valid(payload)))`,
}
var schemaV3 = []string{
	`CREATE TABLE groups (id TEXT PRIMARY KEY,position INTEGER NOT NULL UNIQUE,payload TEXT NOT NULL CHECK(json_valid(payload)))`,
	`CREATE TABLE invites (id TEXT PRIMARY KEY,position INTEGER NOT NULL UNIQUE,payload TEXT NOT NULL CHECK(json_valid(payload)))`,
}
var schema = append([]string{
	`CREATE TABLE users (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','viewer')), disabled INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)), oidc_issuer TEXT NOT NULL DEFAULT '', oidc_subject TEXT NOT NULL DEFAULT '', organization_id TEXT NOT NULL DEFAULT '', role_id TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE sites (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, token_hash TEXT NOT NULL CHECK(length(token_hash)=64), created_at TEXT NOT NULL, organization_id TEXT NOT NULL DEFAULT '',legacy_disabled INTEGER NOT NULL DEFAULT 0 CHECK(legacy_disabled IN (0,1)))`,
	`CREATE TABLE services (id TEXT PRIMARY KEY, position INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, site_id TEXT NOT NULL REFERENCES sites(id), url TEXT NOT NULL, organization_id TEXT NOT NULL DEFAULT '',custom_domain TEXT NOT NULL DEFAULT '',health_path TEXT NOT NULL DEFAULT '',health_method TEXT NOT NULL DEFAULT '',health_expected_status INTEGER NOT NULL DEFAULT 0,connection_mode TEXT NOT NULL DEFAULT '' CHECK(connection_mode IN ('','session')))`,
	`CREATE TABLE user_sites (user_id TEXT NOT NULL REFERENCES users(id), site_id TEXT NOT NULL REFERENCES sites(id), position INTEGER NOT NULL, PRIMARY KEY(user_id,site_id), UNIQUE(user_id,position))`,
	`CREATE TABLE user_services (user_id TEXT NOT NULL REFERENCES users(id), service_id TEXT NOT NULL REFERENCES services(id), position INTEGER NOT NULL, PRIMARY KEY(user_id,service_id), UNIQUE(user_id,position))`,
	`CREATE INDEX services_site ON services(site_id)`,
	resourceDomainIndex,
	`CREATE UNIQUE INDEX users_identity ON users(oidc_issuer,oidc_subject) WHERE oidc_issuer != ''`,
}, append(append(schemaV3, schemaV4...), schemaV7...)...)

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
	if app != applicationID || (version < 1 || version > SchemaVersion) {
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
	// Upgrade all known versions in one transaction, including domain validation.
	if version < SchemaVersion {
		tx, err := b.db.Begin()
		if err != nil {
			return nil, State{}, err
		}
		defer tx.Rollback()
		statements := []string{}
		if version < 2 {
			statements = append(statements,
				"ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1))",
				"ALTER TABLE users ADD COLUMN oidc_issuer TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE users ADD COLUMN oidc_subject TEXT NOT NULL DEFAULT ''",
				"CREATE UNIQUE INDEX users_identity ON users(oidc_issuer,oidc_subject) WHERE oidc_issuer != ''")
		}
		if version < 3 {
			statements = append(statements, schemaV3...)
		}
		if version < 4 {
			statements = append(statements, "ALTER TABLE users ADD COLUMN organization_id TEXT NOT NULL DEFAULT ''", "ALTER TABLE users ADD COLUMN role_id TEXT NOT NULL DEFAULT ''", "ALTER TABLE sites ADD COLUMN organization_id TEXT NOT NULL DEFAULT ''", "ALTER TABLE services ADD COLUMN organization_id TEXT NOT NULL DEFAULT ''")
			statements = append(statements, schemaV4...)
		}
		if version < 5 {
			statements = append(statements, "ALTER TABLE services ADD COLUMN custom_domain TEXT NOT NULL DEFAULT ''")
		}
		if version < 6 {
			statements = append(statements, "ALTER TABLE services ADD COLUMN health_path TEXT NOT NULL DEFAULT ''", "ALTER TABLE services ADD COLUMN health_method TEXT NOT NULL DEFAULT ''", "ALTER TABLE services ADD COLUMN health_expected_status INTEGER NOT NULL DEFAULT 0")
		}
		if version < 7 {
			statements = append(statements, "ALTER TABLE sites ADD COLUMN legacy_disabled INTEGER NOT NULL DEFAULT 0 CHECK(legacy_disabled IN (0,1))")
			statements = append(statements, schemaV7...)
		}
		if version < 9 {
			statements = append(statements, "ALTER TABLE services ADD COLUMN connection_mode TEXT NOT NULL DEFAULT '' CHECK(connection_mode IN ('','session'))")
		}
		statements = append(statements, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion))
		for _, statement := range statements {
			if _, err = tx.Exec(statement); err != nil {
				return nil, State{}, err
			}
		}
		upgraded, err := loadSQLite(tx)
		if err != nil {
			return nil, State{}, err
		}
		if err = Validate(&upgraded); err != nil {
			return nil, State{}, err
		}
		if err = tx.Commit(); err != nil {
			return nil, State{}, err
		}
	}
	s, err := b.load()
	if err != nil {
		return nil, State{}, err
	}
	if err = Validate(&s); err != nil {
		return nil, State{}, err
	}
	if _, err = b.db.Exec(resourceDomainIndex); err != nil {
		return nil, State{}, err
	}
	if e := b.cache(s); e != nil {
		return nil, State{}, e
	}
	ok = true
	return b, s, nil
}
func (b *sqliteBackend) close() error         { return b.db.Close() }
func (b *sqliteBackend) load() (State, error) { return loadSQLite(b.db) }

type sqlReader interface {
	Query(string, ...any) (*sql.Rows, error)
}

func loadSQLite(reader sqlReader) (State, error) {
	s := State{Users: []User{}, Sites: []Site{}, Services: []Service{}}
	rows, err := reader.Query("SELECT id,username,password_hash,role,disabled,oidc_issuer,oidc_subject,organization_id,role_id FROM users ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var u User
		var issuer, subject string
		u.SiteIDs = []string{}
		u.ServiceIDs = []string{}
		if err = rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &issuer, &subject, &u.OrganizationID, &u.RoleID); err != nil {
			_ = rows.Close()
			return s, err
		}
		if issuer != "" || subject != "" {
			u.Identity = &Identity{issuer, subject}
		}
		s.Users = append(s.Users, u)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = reader.Query("SELECT id,name,token_hash,created_at,organization_id,legacy_disabled FROM sites ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var v Site
		var created string
		if err = rows.Scan(&v.ID, &v.Name, &v.TokenHash, &created, &v.OrganizationID, &v.LegacyDisabled); err != nil {
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
	rows, err = reader.Query("SELECT id,name,site_id,url,organization_id,custom_domain,health_path,health_method,health_expected_status,connection_mode FROM services ORDER BY position")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var v Service
		if err = rows.Scan(&v.ID, &v.Name, &v.SiteID, &v.URL, &v.OrganizationID, &v.CustomDomain, &v.HealthPath, &v.HealthMethod, &v.HealthExpectedStatus, &v.ConnectionMode); err != nil {
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
		rows, err = reader.Query("SELECT user_id," + column + " FROM " + table + " ORDER BY user_id,position")
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
	for _, table := range []string{"agents", "organizations", "roles", "groups", "invites"} {
		rows, err = reader.Query("SELECT id,payload FROM " + table + " ORDER BY position")
		if err != nil {
			return s, err
		}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return s, err
			}
			if table == "agents" {
				var v AgentRegistration
				if err = decodeStrict([]byte(raw), &v); err != nil || v.ID != id {
					rows.Close()
					return s, fmt.Errorf("invalid agent payload")
				}
				s.Agents = append(s.Agents, v)
			} else if table == "organizations" {
				var v Organization
				if err = decodeStrict([]byte(raw), &v); err != nil || v.ID != id {
					rows.Close()
					return s, fmt.Errorf("invalid organization payload")
				}
				s.Organizations = append(s.Organizations, v)
			} else if table == "roles" {
				var v Role
				if err = decodeStrict([]byte(raw), &v); err != nil || v.ID != id {
					rows.Close()
					return s, fmt.Errorf("invalid role payload")
				}
				s.Roles = append(s.Roles, v)
			} else if table == "groups" {
				var v Group
				if err = decodeStrict([]byte(raw), &v); err != nil || v.ID != id {
					rows.Close()
					return s, fmt.Errorf("invalid group payload")
				}
				s.Groups = append(s.Groups, v)
			} else {
				var v Invite
				if err = decodeStrict([]byte(raw), &v); err != nil || v.ID != id {
					rows.Close()
					return s, fmt.Errorf("invalid invitation payload")
				}
				s.Invites = append(s.Invites, v)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return s, err
		}
	}
	return s, nil
}
