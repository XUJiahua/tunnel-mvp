package state

import (
	"golang.org/x/crypto/bcrypt"
	"reflect"
	"testing"
)

func TestOfflineAdministratorRecovery(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			initial := sample(t)
			s := mustOpen(t, dir, kind, initial)
			if _, err := ResetAdminPassword(dir, kind, "admin", "new-recovery-password"); err == nil {
				t.Fatal("live Server lock bypassed")
			}
			_ = s.Close()
			for _, user := range []string{"absent", "reader"} {
				if _, err := ResetAdminPassword(dir, kind, user, "new-recovery-password"); err == nil {
					t.Fatal("recovery created/promoted a user")
				}
			}
			reopened := mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(reopened.Snapshot(), initial) {
				t.Fatal("failed recovery changed state")
			}
			_ = reopened.Close()
			if _, err := ResetAdminPassword(dir, kind, "admin", "new-recovery-password"); err != nil {
				t.Fatal(err)
			}
			restored := mustOpen(t, dir, kind, State{})
			next := restored.Snapshot()
			if bcrypt.CompareHashAndPassword([]byte(next.Users[0].PasswordHash), []byte("new-recovery-password")) != nil {
				t.Fatal("new password does not match")
			}
			next.Users[0].PasswordHash = initial.Users[0].PasswordHash
			if !reflect.DeepEqual(next, initial) {
				t.Fatal("recovery changed resources or grants")
			}
		})
	}
}
