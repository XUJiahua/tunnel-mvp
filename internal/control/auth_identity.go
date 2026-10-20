package control

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func (k *keycloakSync) checkShared(ctx context.Context, u User) syncResult {
	s := k.server
	status, owner, err := s.auth.shared.AuthIdentityStart(ctx, u.ID, *u.Identity)
	if err != nil {
		return syncResult{err: err}
	}
	if !owner {
		// A database-fenced flight coalesces checks across nodes. Failed checks do
		// not turn a previous cached success into a new login verification.
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			current, found, _, err := s.auth.shared.AuthIdentityRead(waitCtx, u.ID)
			if err != nil {
				return syncResult{err: err}
			}
			if !found || current.CheckID != status.CheckID {
				return syncResult{err: state.ErrAuthChanged}
			}
			if !current.Pending {
				if !current.CheckSucceeded {
					return syncResult{err: fmt.Errorf("shared identity verification failed")}
				}
				k.rememberShared(u, current)
				return syncResult{enabled: current.Enabled}
			}
			select {
			case <-waitCtx.Done():
				return syncResult{err: waitCtx.Err()}
			case <-tick.C:
			}
		}
	}
	result := k.fetch(ctx, u)
	s.stats.identitySyncChecks.Add(1)
	if result.err == nil && ctx.Err() != nil {
		result.err = ctx.Err()
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	completed, applied, finishErr := s.auth.shared.AuthIdentityFinish(finishCtx, u.ID, status.CheckID, result.err == nil, result.enabled, k.maxStale)
	cancel()
	if result.err != nil {
		s.stats.identitySyncErrors.Add(1)
		slog.Warn("Keycloak identity status check failed", "user_id", u.ID, "error", result.err)
		return result
	}
	if finishErr != nil || !applied {
		if finishErr == nil {
			finishErr = state.ErrAuthChanged
		}
		return syncResult{err: finishErr}
	}
	s.stats.identitySyncRevocations.Add(completed.Revoked)
	k.rememberShared(u, completed)
	if !completed.Enabled {
		s.audit("identity_sync_session_revoked", u.Username, "", u.ID, map[string]any{"reason": "disabled_or_deleted", "sessions": completed.Revoked})
	}
	return syncResult{enabled: completed.Enabled}
}
func (k *keycloakSync) rememberShared(u User, status state.AuthIdentityStatus) {
	k.mu.Lock()
	k.records[u.ID] = syncRecord{identity: *u.Identity, enabled: status.Enabled, verified: time.Now()}
	k.mu.Unlock()
	if !status.Enabled {
		k.server.cancelRequests(func(r liveRequest) bool { return r.userID == u.ID && r.method == "oidc" })
		k.server.pruneSessionPools()
	}
}
