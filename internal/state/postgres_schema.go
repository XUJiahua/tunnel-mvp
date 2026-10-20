package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"
)

const Postgres = "postgres"
const PostgresSchemaVersion = 2
const pgNamespace = 1414876748 // TUNL; advisory keys are scoped to this database.
const pgRuntimeKey = 1
const pgSchemaKey = 2

// Entity JSON is canonical. Stored generated columns provide database-enforced
// identities/ownership without maintaining a second mutable scalar projection.
var pgEntityKinds = []string{"organizations", "roles", "users", "sites", "services", "groups", "invitations", "agents"}
var pgRelations = []string{"user_sites", "user_services", "group_members", "group_sites", "group_services", "invite_sites", "invite_services", "invite_groups", "credentials"}

func pgTable(kind string) string { return "tunnel_state." + kind }

const pgOwner = `organization_id TEXT GENERATED ALWAYS AS (coalesce(payload->>'organizationID','')) STORED NOT NULL REFERENCES tunnel_state.organizations(id) DEFERRABLE INITIALLY DEFERRED`

// PostgreSQL also reserves every legacy site credential globally. Local stores
// historically allowed two sites to reuse a hash; require rotating those before
// import rather than silently dropping or changing either credential.
func validatePostgresState(st *State) error {
	if err := Validate(st); err != nil {
		return err
	}
	tokens := map[string]bool{}
	for _, site := range st.Sites {
		if tokens[site.TokenHash] {
			return fmt.Errorf("PostgreSQL requires distinct site credential hashes; rotate duplicate legacy credentials before migrating")
		}
		tokens[site.TokenHash] = true
	}
	return nil
}

func pgCreateSchema(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS tunnel_state`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.meta (singleton BOOLEAN PRIMARY KEY CHECK(singleton), schema_version INTEGER NOT NULL, model_version INTEGER NOT NULL, instance_id TEXT NOT NULL, revision BIGINT NOT NULL CHECK(revision>=0),runtime_owner TEXT NOT NULL DEFAULT '')`,
	}
	base := func(kind, extra string) string {
		return `CREATE TABLE IF NOT EXISTS ` + pgTable(kind) + ` (id TEXT PRIMARY KEY,position BIGINT NOT NULL UNIQUE DEFERRABLE INITIALLY DEFERRED,payload JSONB NOT NULL CHECK(jsonb_typeof(payload)='object' AND payload ? 'id' AND jsonb_typeof(payload->'id')='string' AND payload->>'id'=id)` + extra + `)`
	}
	statements = append(statements,
		base("organizations", ``),
		base("roles", `,`+pgOwner+`,UNIQUE(id,organization_id)`),
		base("users", `,`+pgOwner+`,username TEXT GENERATED ALWAYS AS (payload->>'username') STORED NOT NULL UNIQUE DEFERRABLE INITIALLY DEFERRED,role TEXT GENERATED ALWAYS AS (payload->>'role') STORED NOT NULL CHECK(role IN ('admin','viewer')),role_id TEXT GENERATED ALWAYS AS (nullif(payload->>'roleID','')) STORED,oidc_issuer TEXT GENERATED ALWAYS AS (nullif(payload->'identity'->>'issuer','')) STORED,oidc_subject TEXT GENERATED ALWAYS AS (nullif(payload->'identity'->>'subject','')) STORED,CHECK((oidc_issuer IS NULL)=(oidc_subject IS NULL)),UNIQUE(oidc_issuer,oidc_subject) DEFERRABLE INITIALLY DEFERRED,UNIQUE(id,organization_id),FOREIGN KEY(role_id,organization_id) REFERENCES tunnel_state.roles(id,organization_id) DEFERRABLE INITIALLY DEFERRED`),
		base("sites", `,`+pgOwner+`,UNIQUE(id,organization_id)`),
		base("services", `,`+pgOwner+`,site_id TEXT GENERATED ALWAYS AS (payload->>'siteID') STORED NOT NULL,custom_domain TEXT GENERATED ALWAYS AS (nullif(payload->>'customDomain','')) STORED UNIQUE DEFERRABLE INITIALLY DEFERRED,UNIQUE(id,organization_id),FOREIGN KEY(site_id,organization_id) REFERENCES tunnel_state.sites(id,organization_id) DEFERRABLE INITIALLY DEFERRED`),
		base("groups", `,`+pgOwner+`,UNIQUE(id,organization_id)`),
		base("invitations", `,`+pgOwner+`,token_hash TEXT GENERATED ALWAYS AS (payload->>'tokenHash') STORED NOT NULL UNIQUE DEFERRABLE INITIALLY DEFERRED,UNIQUE(id,organization_id)`),
		base("agents", `,site_id TEXT GENERATED ALWAYS AS (payload->>'siteID') STORED NOT NULL REFERENCES tunnel_state.sites(id) DEFERRABLE INITIALLY DEFERRED`),
	)
	for _, relation := range []struct{ name, leftTable, left, rightTable, right string }{
		{"user_sites", "users", "user_id", "sites", "site_id"}, {"user_services", "users", "user_id", "services", "service_id"},
		{"group_members", "groups", "group_id", "users", "user_id"}, {"group_sites", "groups", "group_id", "sites", "site_id"}, {"group_services", "groups", "group_id", "services", "service_id"},
		{"invite_sites", "invitations", "invite_id", "sites", "site_id"}, {"invite_services", "invitations", "invite_id", "services", "service_id"}, {"invite_groups", "invitations", "invite_id", "groups", "group_id"},
	} {
		statements = append(statements, `CREATE TABLE IF NOT EXISTS `+pgTable(relation.name)+` (`+relation.left+` TEXT NOT NULL,`+relation.right+` TEXT NOT NULL,organization_id TEXT NOT NULL,position BIGINT NOT NULL,PRIMARY KEY(`+relation.left+`,`+relation.right+`),UNIQUE(`+relation.left+`,position),FOREIGN KEY(`+relation.left+`,organization_id) REFERENCES `+pgTable(relation.leftTable)+`(id,organization_id) DEFERRABLE INITIALLY DEFERRED,FOREIGN KEY(`+relation.right+`,organization_id) REFERENCES `+pgTable(relation.rightTable)+`(id,organization_id) DEFERRABLE INITIALLY DEFERRED)`)
	}
	statements = append(statements,
		`CREATE TABLE IF NOT EXISTS tunnel_state.credentials (owner_id TEXT PRIMARY KEY,token_hash TEXT NOT NULL UNIQUE DEFERRABLE INITIALLY DEFERRED CHECK(length(token_hash)=64),site_id TEXT NOT NULL REFERENCES tunnel_state.sites(id) DEFERRABLE INITIALLY DEFERRED,agent_id TEXT REFERENCES tunnel_state.agents(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE INDEX IF NOT EXISTS group_members_user ON tunnel_state.group_members(user_id,group_id)`,
		`CREATE INDEX IF NOT EXISTS services_site ON tunnel_state.services(site_id,position)`,
	)
	for _, kind := range []string{"roles", "users", "sites", "services", "groups", "invitations"} {
		statements = append(statements, `CREATE INDEX IF NOT EXISTS `+kind+`_organization ON `+pgTable(kind)+`(organization_id,position)`)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return pgCreateAuthSchema(ctx, tx)
}
func pgVisit(st State, kind string, visit func(int, any) bool) {
	if kind == "agents" {
		visitSlice(st.Agents, visit)
	} else {
		visitDirectory(st, kind, visit)
	}
}
func pgAppend(st *State, value any) {
	if v, ok := value.(AgentRegistration); ok {
		st.Agents = append(st.Agents, v)
	} else {
		appendDirectory(st, value)
	}
}
func pgPayload(kind, id string, raw []byte) (any, error) {
	if kind == "agents" {
		var v AgentRegistration
		if err := decodeStrict(raw, &v); err != nil || v.ID != id {
			return nil, fmt.Errorf("invalid agent projection")
		}
		return v, nil
	}
	if kind == "users" {
		var v User
		if err := decodeStrict(raw, &v); err != nil || v.ID != id {
			return nil, fmt.Errorf("invalid user projection")
		}
		return v, nil
	}
	if kind == "sites" {
		var v Site
		if err := decodeStrict(raw, &v); err != nil || v.ID != id {
			return nil, fmt.Errorf("invalid site projection")
		}
		return v, nil
	}
	if kind == "services" {
		var v Service
		if err := decodeStrict(raw, &v); err != nil || v.ID != id {
			return nil, fmt.Errorf("invalid service projection")
		}
		return v, nil
	}
	return directoryPayload(kind, id, string(raw))
}
func pgReadState(ctx context.Context, tx *sql.Tx) (State, error) {
	var st State
	for _, kind := range pgEntityKinds {
		rows, err := tx.QueryContext(ctx, `SELECT id,payload FROM `+pgTable(kind)+` WHERE id<>'' ORDER BY position`)
		if err != nil {
			return State{}, err
		}
		for rows.Next() {
			var id string
			var raw []byte
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var v any
			v, err = pgPayload(kind, id, raw)
			if err != nil {
				break
			}
			pgAppend(&st, v)
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return State{}, err
		}
	}
	if err := Validate(&st); err != nil {
		return State{}, fmt.Errorf("invalid PostgreSQL configuration")
	}
	return st, nil
}
func pgCopy(ctx context.Context, tx *sql.Tx, table string, columns []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, pq.CopyInSchema("tunnel_state", table, columns...))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err = stmt.ExecContext(ctx, row...); err != nil {
			return err
		}
	}
	if _, err = stmt.ExecContext(ctx); err != nil {
		return err
	}
	return stmt.Close()
}

// Full-state replacement is deliberately serialized by the revision row lock.
// All constraints remain active and deferred; no TRUNCATE/CASCADE or FK disable.
func pgWriteState(ctx context.Context, tx *sql.Tx, st State) error {
	for _, kind := range pgRelations {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+pgTable(kind)); err != nil {
			return err
		}
	}
	for i := len(pgEntityKinds) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+pgTable(pgEntityKinds[i])); err != nil {
			return err
		}
	}
	for _, kind := range pgEntityKinds {
		rows := [][]any{}
		if kind == "organizations" {
			rows = append(rows, []any{"", -1, `{"id":"","name":"Default"}`})
		}
		var encodeErr error
		pgVisit(st, kind, func(position int, v any) bool {
			raw, err := json.Marshal(v)
			if err != nil {
				encodeErr = err
				return false
			}
			var id struct {
				ID string `json:"id"`
			}
			if err = json.Unmarshal(raw, &id); err != nil {
				encodeErr = err
				return false
			}
			rows = append(rows, []any{id.ID, position, string(raw)})
			return true
		})
		if encodeErr != nil {
			return encodeErr
		}
		if err := pgCopy(ctx, tx, kind, []string{"id", "position", "payload"}, rows); err != nil {
			return err
		}
	}
	relations := map[string][][]any{}
	add := func(table, left, org string, ids []string) {
		for position, id := range ids {
			relations[table] = append(relations[table], []any{left, id, org, position})
		}
	}
	for _, u := range st.Users {
		add("user_sites", u.ID, u.OrganizationID, u.SiteIDs)
		add("user_services", u.ID, u.OrganizationID, u.ServiceIDs)
	}
	for _, g := range st.Groups {
		add("group_members", g.ID, g.OrganizationID, g.MemberIDs)
		add("group_sites", g.ID, g.OrganizationID, g.SiteIDs)
		add("group_services", g.ID, g.OrganizationID, g.ServiceIDs)
	}
	for _, v := range st.Invites {
		add("invite_sites", v.ID, v.OrganizationID, v.SiteIDs)
		add("invite_services", v.ID, v.OrganizationID, v.ServiceIDs)
		add("invite_groups", v.ID, v.OrganizationID, v.GroupIDs)
	}
	for _, relation := range []struct{ table, left, right string }{{"user_sites", "user_id", "site_id"}, {"user_services", "user_id", "service_id"}, {"group_members", "group_id", "user_id"}, {"group_sites", "group_id", "site_id"}, {"group_services", "group_id", "service_id"}, {"invite_sites", "invite_id", "site_id"}, {"invite_services", "invite_id", "service_id"}, {"invite_groups", "invite_id", "group_id"}} {
		if err := pgCopy(ctx, tx, relation.table, []string{relation.left, relation.right, "organization_id", "position"}, relations[relation.table]); err != nil {
			return err
		}
	}
	credentials := [][]any{}
	for _, site := range st.Sites {
		credentials = append(credentials, []any{"site:" + site.ID, site.TokenHash, site.ID, nil})
	}
	for _, agent := range st.Agents {
		credentials = append(credentials, []any{"agent:" + agent.ID, agent.TokenHash, agent.SiteID, agent.ID})
	}
	return pgCopy(ctx, tx, "credentials", []string{"owner_id", "token_hash", "site_id", "agent_id"}, credentials)
}
