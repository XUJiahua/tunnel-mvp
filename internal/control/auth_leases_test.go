package control

import (
	"context"
	"testing"
	"time"
)

func TestAuthorizationLeaseCannotRenewAfterDelayedTimerDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := newAuthLease(ctx, cancel, time.Now().Add(30*time.Millisecond))
	defer lease.stop()
	// Hold back delivery, as when an expired timer is queued behind other work.
	// The stored deadline remains authoritative even without the callback.
	lease.mu.Lock()
	lease.timer.Stop()
	until := lease.until
	lease.mu.Unlock()
	time.Sleep(time.Until(until) + 10*time.Millisecond)
	lease.renew(time.Now().Add(time.Hour))
	if ctx.Err() == nil {
		t.Fatal("expired authorization was resurrected before timer delivery")
	}
}
func TestAuthorizationLeaseRejectsAlreadyExpiredProof(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := newAuthLease(ctx, cancel, time.Now().Add(time.Second))
	defer lease.stop()
	lease.renew(time.Now().Add(-time.Millisecond))
	if ctx.Err() == nil {
		t.Fatal("late database proof extended authorization")
	}
	lease.renew(time.Now().Add(time.Hour))
	if ctx.Err() == nil {
		t.Fatal("cancelled authorization was resurrected")
	}
}
