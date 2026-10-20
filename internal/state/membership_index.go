package state

import (
	"database/sql"
	"slices"
)

// JSON remains canonical; this derived index is rebuilt after validation and
// maintained in the same commit. It is not an independent authorization source.
func (b *sqliteBackend) prepareMembershipIndex(st State) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS group_members (group_id TEXT NOT NULL,user_id TEXT NOT NULL,position INTEGER NOT NULL,PRIMARY KEY(group_id,user_id))`,
		`CREATE INDEX IF NOT EXISTS group_members_user ON group_members(user_id,group_id)`,
		`DELETE FROM group_members`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	if err = saveMembershipDelta(tx, nil, membershipRows(st.Groups)); err != nil {
		return err
	}
	return tx.Commit()
}
func membershipRows(groups []Group) map[string][]string {
	out := map[string][]string{}
	for _, g := range groups {
		out[g.ID] = append([]string{}, g.MemberIDs...)
	}
	return out
}
func saveMembershipDelta(tx *sql.Tx, before, after map[string][]string) error {
	for id, old := range before {
		if fresh, ok := after[id]; !ok || !slices.Equal(old, fresh) {
			if _, err := tx.Exec("DELETE FROM group_members WHERE group_id=?", id); err != nil {
				return err
			}
		}
	}
	for id, members := range after {
		if old, ok := before[id]; ok && slices.Equal(old, members) {
			continue
		}
		for position, member := range members {
			if _, err := tx.Exec("INSERT INTO group_members VALUES(?,?,?)", id, member, position); err != nil {
				return err
			}
		}
	}
	return nil
}
