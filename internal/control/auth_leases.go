package control

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const authorizationLease = 5 * time.Second

// A delayed SQL reply can only renew through readStarted+lease, never through
// replyArrival+lease. An expired timer cannot be resurrected by a later reply.
type authLease struct {
	mu         sync.Mutex
	timer      *time.Timer
	cancel     context.CancelFunc
	ctx        context.Context
	generation uint64
	until      time.Time
	stopped    bool
}

func newAuthLease(ctx context.Context, cancel context.CancelFunc, until time.Time) *authLease {
	l := &authLease{ctx: ctx, cancel: cancel}
	l.renew(until)
	return l
}
func (l *authLease) renew(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped || l.ctx.Err() != nil {
		return
	}
	// Timer delivery can lag behind its deadline (scheduler pause/contention).
	// A queued expiration must not lose a race to a delayed renewal.
	if !l.until.IsZero() && !time.Now().Before(l.until) {
		l.stopped = true
		if l.timer != nil {
			l.timer.Stop()
		}
		l.cancel()
		return
	}
	l.until = until
	l.generation++
	generation := l.generation
	if l.timer != nil {
		l.timer.Stop()
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		l.stopped = true
		l.cancel()
		return
	}
	l.timer = time.AfterFunc(remaining, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.stopped && l.generation == generation {
			l.stopped = true
			l.cancel()
		}
	})
}
func (l *authLease) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	l.generation++
	if l.timer != nil {
		l.timer.Stop()
	}
}
func (s *Server) startSharedAuth(ctx context.Context) {
	if s.auth.shared == nil {
		return
	}
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		maintenance := time.NewTicker(time.Minute)
		defer maintenance.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.refreshSharedAuth(ctx)
			case <-maintenance.C:
				if err := s.auth.shared.AuthMaintenance(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("shared authentication maintenance unavailable")
				}
			}
		}
	}()
}
func (s *Server) refreshSharedAuth(ctx context.Context) {
	// Capture exactly the requests covered by this proof. Newly registered flows
	// keep their own initial lease and are picked up by the next iteration.
	s.liveMu.Lock()
	live := make(map[string]liveRequest, len(s.live))
	for id, r := range s.live {
		live[id] = r
	}
	s.liveMu.Unlock()
	accesses, sessions, services := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range live {
		if r.accessKey != "" {
			accesses[r.accessKey] = true
		}
		sessions[r.sessionKey] = true
		services[r.serviceID] = true
	}
	pools := s.snapshotPools()
	for _, p := range pools {
		services[p.service.ID] = true
		if p.sessionKey != "" && p.sessionKey != "probe" {
			sessions[p.sessionKey] = true
		}
	}
	keys := func(set map[string]bool) []string {
		out := make([]string, 0, len(set))
		for k := range set {
			out = append(out, k)
		}
		return out
	}
	started := time.Now()
	snapshot, err := s.auth.shared.AuthSnapshotForKeys(ctx, keys(accesses), keys(sessions), keys(services))
	if err != nil {
		return
	} // Existing timers expire independently of this goroutine.
	users := map[string]User{}
	targets := map[string]Service{}
	for _, u := range snapshot.State.Users {
		users[u.ID] = effectiveUser(snapshot.State, u)
	}
	for _, svc := range snapshot.State.Services {
		targets[svc.ID] = svc
	}
	for _, r := range live {
		sess, exists := snapshot.Data.Sessions[r.sessionKey]
		user, userFound := users[r.userID]
		svc, serviceFound := targets[r.serviceID]
		valid := exists && sess.UserID == r.userID && userFound && !user.Disabled && serviceFound && sameRoute(r.route, svc) && s.authIdentityAllowed(snapshot.Data, sess, snapshot.Now)
		if r.accessKey != "" {
			grant, ok := snapshot.Data.Access[r.accessKey]
			valid = valid && ok && grant.SessionKey == r.sessionKey && grant.ServiceID == r.serviceID && canAccessService(user, svc)
		} else {
			valid = valid && (canAccessService(user, svc) || canManage(user, svc.OrganizationID, "applications:manage"))
		}
		if !valid {
			r.cancel()
			continue
		}
		if r.lease != nil {
			remaining := sess.Expires.Sub(snapshot.Now)
			if r.accessKey != "" {
				if grant := snapshot.Data.Access[r.accessKey]; grant.Expires.Sub(snapshot.Now) < remaining {
					remaining = grant.Expires.Sub(snapshot.Now)
				}
			}
			if sess.Method == "oidc" && s.identitySync != nil {
				if status := snapshot.Data.Identities[sess.UserID]; status.ValidUntil.Sub(snapshot.Now) < remaining {
					remaining = status.ValidUntil.Sub(snapshot.Now)
				}
			}
			if remaining > authorizationLease {
				remaining = authorizationLease
			}
			r.lease.renew(started.Add(remaining))
		}
	}
	s.poolMu.Lock()
	for key, pool := range pools {
		// Only retire the exact transport covered by this proof. A request may
		// have created or replaced the same key while the database read ran.
		if current, ok := s.pools[key]; !ok || current.transport != pool.transport {
			continue
		}
		svc, found := targets[pool.service.ID]
		valid := found && sameRoute(pool.service, svc)
		if pool.sessionKey != "" && pool.sessionKey != "probe" {
			sess, exists := snapshot.Data.Sessions[pool.sessionKey]
			user, ok := users[sess.UserID]
			valid = valid && exists && ok && !user.Disabled && s.authIdentityAllowed(snapshot.Data, sess, snapshot.Now) && canAccessService(user, svc)
		}
		if !valid {
			pool.transport.CloseIdleConnections()
			delete(s.pools, key)
		}
	}
	s.poolMu.Unlock()
	// Polling is a durable invalidation hint. Every round already re-proved the
	// authority above, including when notifications are lost or retention has gaps.
	pollCtx, pollCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, pollErr := s.auth.shared.AuthPoll(pollCtx, s.nodeID, 200)
	pollCancel()
	if pollErr != nil {
		return
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_ = s.auth.shared.AuthAcknowledge(ackCtx, s.nodeID, snapshot.EventID)
	ackCancel()
}
func sameRoute(a, b Service) bool {
	return a.ID == b.ID && a.SiteID == b.SiteID && a.URL == b.URL && a.CustomDomain == b.CustomDomain && a.ConnectionMode == b.ConnectionMode && a.OrganizationID == b.OrganizationID
}
