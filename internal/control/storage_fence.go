package control

import (
	"context"
	"log/slog"
)

func (s *Server) startStorageFence(ctx context.Context) {
	source, ok := s.store.Repository.(interface{ Failure() <-chan struct{} })
	if !ok {
		return
	}
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		select {
		case <-ctx.Done():
			return
		case <-source.Failure():
		}
		s.storageFenced.Store(true)
		// Never acquire accessMu or read configuration here: a mutation may hold
		// those locks while waiting for the failed database connection.
		s.cancelRequests(func(liveRequest) bool { return true })
		s.auth.mu.Lock()
		clear(s.auth.Sessions)
		clear(s.auth.Tickets)
		clear(s.auth.Access)
		s.auth.mu.Unlock()
		s.tunnelMu.Lock()
		s.closing = true
		for _, c := range s.connections {
			c.closing.Store(true)
			c.cancel()
		}
		s.tunnelMu.Unlock()
		s.poolMu.Lock()
		for id, p := range s.pools {
			p.transport.CloseIdleConnections()
			delete(s.pools, id)
		}
		s.poolMu.Unlock()
		slog.Error("PostgreSQL runtime lease lost; control plane fenced; stop Server before offline recovery")
	}()
}
