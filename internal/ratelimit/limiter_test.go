package ratelimit

import (
	"testing"
	"time"
)

func TestAllow_BurstThenBlocked(t *testing.T) {
	l := New(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("expected allow on attempt %d", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("expected 4th attempt to be blocked")
	}
}

func TestAllow_DistinctKeysIndependent(t *testing.T) {
	l := New(1, time.Minute)
	if !l.Allow("a") {
		t.Fatal("expected first attempt for a to be allowed")
	}
	if !l.Allow("b") {
		t.Fatal("expected first attempt for b to be allowed, independent of a")
	}
	if l.Allow("a") {
		t.Fatal("expected second attempt for a to be blocked")
	}
}

func TestAllow_RecoversAfterWindow(t *testing.T) {
	l := New(1, 30*time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("expected first attempt to be allowed")
	}
	if l.Allow("a") {
		t.Fatal("expected second attempt within the window to be blocked")
	}
	time.Sleep(40 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("expected attempt after the window elapsed to be allowed again")
	}
}

func TestAllow_NonPositiveMaxDisablesLimiting(t *testing.T) {
	l := New(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !l.Allow("a") {
			t.Fatalf("expected unlimited allow with max<=0, blocked at attempt %d", i)
		}
	}
}

func TestGC_DropsStaleKeys(t *testing.T) {
	l := New(1, 20*time.Millisecond)
	l.Allow("a")
	time.Sleep(30 * time.Millisecond)
	l.GC(time.Now())

	l.mu.Lock()
	_, exists := l.hits["a"]
	l.mu.Unlock()
	if exists {
		t.Fatal("expected stale key to be dropped by GC")
	}
}

func TestGC_KeepsFreshKeys(t *testing.T) {
	l := New(5, time.Minute)
	l.Allow("a")
	l.GC(time.Now())

	l.mu.Lock()
	_, exists := l.hits["a"]
	l.mu.Unlock()
	if !exists {
		t.Fatal("expected fresh key to survive GC")
	}
}
