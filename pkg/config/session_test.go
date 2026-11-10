package config

import (
	"testing"
	"time"
)

func TestSessionRegistry_BasicLifecycle(t *testing.T) {
	sr := NewSessionRegistry(50 * time.Millisecond)

	// Non-existent session
	if s, ok := sr.GetSession("sess-1"); ok || s != nil {
		t.Fatalf("expected nil for non-existent session, got %+v", s)
	}

	// Create session
	sess := sr.GetOrCreateSession("sess-1", "app-1", "lock")
	if sess == nil || sess.SessionID != "sess-1" || sess.AppID != "app-1" || sess.Policy != "lock" {
		t.Fatalf("unexpected session creation: %+v", sess)
	}

	// Set model & task
	sr.SetModel("sess-1", "gemini-3.8-flash")
	sr.SetTask("sess-1", "code_review")

	retrieved, ok := sr.GetSession("sess-1")
	if !ok || retrieved.Model != "gemini-3.8-flash" || retrieved.CurrentTask != "code_review" {
		t.Fatalf("retrieved session mismatch: %+v", retrieved)
	}

	// Increment turn
	turn1 := sr.IncrementTurn("sess-1")
	turn2 := sr.IncrementTurn("sess-1")
	if turn1 != 1 || turn2 != 2 {
		t.Fatalf("expected turns 1 and 2, got %d, %d", turn1, turn2)
	}

	// Check TTL expiration
	time.Sleep(70 * time.Millisecond)
	if _, ok := sr.GetSession("sess-1"); ok {
		t.Fatalf("expected session to be expired after TTL")
	}

	// Test PruneExpired and Clear
	sr.GetOrCreateSession("sess-2", "app-2", "lock")
	sr.Clear()
	if _, ok := sr.GetSession("sess-2"); ok {
		t.Fatalf("expected session to be cleared")
	}
}

func TestSessionRegistry_StickyTurnPolicy(t *testing.T) {
	sr := NewSessionRegistry(5 * time.Minute)
	sess := sr.GetOrCreateSession("sess-sticky", "app-1", "sticky-turn")

	// Initial turn: simple
	m, tier, up := sess.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
	if m != "gemini-3.5-flash-lite" || tier != "simple" || up {
		t.Fatalf("expected initial simple lock, got %s (%s) up=%v", m, tier, up)
	}

	// Immediate upgrade to complex
	m, tier, up = sess.ApplyStickyTurn("gemini-3.1-pro", "complex")
	if m != "gemini-3.1-pro" || tier != "complex" || !up {
		t.Fatalf("expected immediate upgrade to complex, got %s (%s) up=%v", m, tier, up)
	}

	// 3 subsequent simple turns stay locked on complex
	for i := 1; i <= 3; i++ {
		m, tier, up = sess.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
		if m != "gemini-3.1-pro" || tier != "complex" || up {
			t.Fatalf("sticky turn %d expected gemini-3.1-pro, got %s (%s)", i, m, tier)
		}
	}

	// After 3 turns since upgrade, a simple turn triggers controlled downgrade
	m, tier, up = sess.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
	if m != "gemini-3.5-flash-lite" || tier != "simple" || up {
		t.Fatalf("expected controlled downgrade to gemini-3.5-flash-lite, got %s (%s)", m, tier)
	}
}

