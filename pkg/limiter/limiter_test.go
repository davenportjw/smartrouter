package limiter

import (
	"context"
	"testing"
	"time"
)

// --- Regression Tests: Rate limiter state preservation ---

func TestUpdateLimiter_SameConfig_PreservesInstance(t *testing.T) {
	// Regression test: Calling UpdateLimiter repeatedly with the same config values
	// must NOT recreate the rate limiter. Previously, every call replaced the limiter
	// with a new instance, resetting the token bucket and making rate limiting
	// completely non-functional.

	registry := NewRateLimiterRegistry()

	// First call creates the limiter
	registry.UpdateLimiter("app-1", 60, 40000, "medium")

	registry.mu.RLock()
	first := registry.limiters["app-1"]
	registry.mu.RUnlock()

	if first == nil {
		t.Fatal("expected limiter to be created after first UpdateLimiter call")
	}

	// Subsequent calls with the SAME config should return the same instance
	for i := 0; i < 10; i++ {
		registry.UpdateLimiter("app-1", 60, 40000, "medium")
	}

	registry.mu.RLock()
	after := registry.limiters["app-1"]
	registry.mu.RUnlock()

	if first != after {
		t.Errorf("UpdateLimiter with identical config recreated the limiter (pointer changed from %p to %p); token bucket state was lost", first, after)
	}
}

func TestUpdateLimiter_DifferentConfig_RecreatesInstance(t *testing.T) {
	// When the config actually changes, the limiter SHOULD be recreated
	// with the new values.

	registry := NewRateLimiterRegistry()

	registry.UpdateLimiter("app-1", 60, 40000, "medium")

	registry.mu.RLock()
	first := registry.limiters["app-1"]
	registry.mu.RUnlock()

	// Change RPM
	registry.UpdateLimiter("app-1", 120, 40000, "medium")

	registry.mu.RLock()
	afterRPMChange := registry.limiters["app-1"]
	registry.mu.RUnlock()

	if first == afterRPMChange {
		t.Error("UpdateLimiter with changed RPM should recreate the limiter, but pointer is the same")
	}

	// Change TPM
	registry.UpdateLimiter("app-1", 120, 80000, "medium")

	registry.mu.RLock()
	afterTPMChange := registry.limiters["app-1"]
	registry.mu.RUnlock()

	if afterRPMChange == afterTPMChange {
		t.Error("UpdateLimiter with changed TPM should recreate the limiter, but pointer is the same")
	}

	// Change priority
	registry.UpdateLimiter("app-1", 120, 80000, "high")

	registry.mu.RLock()
	afterPriorityChange := registry.limiters["app-1"]
	registry.mu.RUnlock()

	if afterTPMChange == afterPriorityChange {
		t.Error("UpdateLimiter with changed priority should recreate the limiter, but pointer is the same")
	}
}

func TestRateLimiter_EnforcesLimits(t *testing.T) {
	// Regression test: The rate limiter must actually enforce RPM limits.
	// Previously, because UpdateLimiter reset the token bucket on every request,
	// limits were never enforced.

	registry := NewRateLimiterRegistry()

	// Set an extremely low RPM (2 per minute) so we can easily exceed it
	registry.UpdateLimiter("app-limited", 2, 100000, "low")

	ctx := context.Background()

	// First 2 requests should be allowed (burst size = RPM)
	for i := 0; i < 2; i++ {
		allowed, _ := registry.EvaluateLimit(ctx, "app-limited", 1)
		if !allowed {
			t.Errorf("request %d should be allowed (within RPM burst), but was rejected", i+1)
		}
	}

	// The next request should be rejected because low-priority gets no queueing
	allowed, _ := registry.EvaluateLimit(ctx, "app-limited", 1)
	if allowed {
		t.Error("request exceeding RPM limit should be rejected for low-priority app, but was allowed")
	}
}

func TestRateLimiter_EnforcesLimits_WithRepeatedUpdateLimiter(t *testing.T) {
	// The critical regression: call UpdateLimiter before each request (as ServeHTTP does)
	// and verify limits still work.

	registry := NewRateLimiterRegistry()
	ctx := context.Background()

	// Simulate what ServeHTTP does: call UpdateLimiter then EvaluateLimit on every request
	allowedCount := 0
	totalRequests := 10

	for i := 0; i < totalRequests; i++ {
		// This mirrors the pattern in ServeHTTP: update then evaluate
		registry.UpdateLimiter("app-enforce", 3, 100000, "low")
		allowed, _ := registry.EvaluateLimit(ctx, "app-enforce", 1)
		if allowed {
			allowedCount++
		}
	}

	// With RPM=3 and low priority (no queueing), at most 3 requests should be allowed
	// (the initial burst). If UpdateLimiter resets state, all 10 would be allowed.
	if allowedCount > 3 {
		t.Errorf("expected at most 3 requests allowed (RPM=3, low priority), but %d out of %d were allowed; rate limiter state is being reset on each UpdateLimiter call", allowedCount, totalRequests)
	}
	if allowedCount == 0 {
		t.Error("expected at least some requests to be allowed, but none were")
	}
}

func TestRateLimiter_HighPriority_QueuesBeforeRejection(t *testing.T) {
	// High-priority requests should be queued briefly (up to 5s) before rejection,
	// rather than rejected immediately.

	registry := NewRateLimiterRegistry()

	// RPM=60 gives a token refill rate of 1/s, so after exhausting the burst
	// the next reservation delay is ~1s which is well within the 5s queue window.
	registry.UpdateLimiter("app-high", 60, 100000, "high")

	ctx := context.Background()

	// Exhaust the burst (burst size = RPM = 60)
	for i := 0; i < 60; i++ {
		allowed, _ := registry.EvaluateLimit(ctx, "app-high", 1)
		if !allowed {
			t.Fatalf("request %d should be allowed (within burst)", i+1)
		}
	}

	// Next request for high-priority should be allowed with a delay (queued)
	allowed, delay := registry.EvaluateLimit(ctx, "app-high", 1)

	if !allowed {
		t.Error("high-priority request should be queued (allowed with delay) when over RPM, not rejected immediately")
	}
	if delay <= 0 {
		t.Error("high-priority request over RPM should have a positive delay for queueing")
	}
	if delay > 5*time.Second {
		t.Errorf("high-priority queue delay should be under 5s, got %v", delay)
	}
}

func TestRateLimiter_LowPriority_RejectsImmediately(t *testing.T) {
	// Low-priority requests should be rejected immediately when over the limit,
	// with no queueing delay.

	registry := NewRateLimiterRegistry()

	registry.UpdateLimiter("app-low", 2, 100000, "low")

	ctx := context.Background()

	// Exhaust the burst
	for i := 0; i < 2; i++ {
		registry.EvaluateLimit(ctx, "app-low", 1)
	}

	// Low-priority should be rejected immediately
	allowed, delay := registry.EvaluateLimit(ctx, "app-low", 1)

	if allowed {
		t.Error("low-priority request over RPM should be rejected immediately, but was allowed")
	}
	if delay != 0 {
		t.Errorf("low-priority rejection should have zero delay, got %v", delay)
	}
}

func TestRateLimiter_MediumPriority_QueuesBriefly(t *testing.T) {
	// Medium-priority requests can wait up to 2 seconds.

	registry := NewRateLimiterRegistry()

	// RPM=60 gives a token refill rate of 1/s, so after exhausting the burst
	// the next reservation delay is ~1s which is within the 2s queue window.
	registry.UpdateLimiter("app-med", 60, 100000, "medium")

	ctx := context.Background()

	// Exhaust the burst (burst size = RPM = 60)
	for i := 0; i < 60; i++ {
		registry.EvaluateLimit(ctx, "app-med", 1)
	}

	// Medium-priority should be queued with a shorter delay than high
	allowed, delay := registry.EvaluateLimit(ctx, "app-med", 1)

	if !allowed {
		t.Error("medium-priority request should be queued (allowed with delay) when slightly over RPM, not rejected immediately")
	}
	if delay <= 0 {
		t.Error("medium-priority request over RPM should have a positive delay")
	}
	if delay > 2*time.Second {
		t.Errorf("medium-priority queue delay should be under 2s, got %v", delay)
	}
}

func TestRateLimiter_SeparateApps_IndependentLimits(t *testing.T) {
	// Different apps should have independent rate limiters that don't interfere.

	registry := NewRateLimiterRegistry()

	registry.UpdateLimiter("app-a", 2, 100000, "low")
	registry.UpdateLimiter("app-b", 2, 100000, "low")

	ctx := context.Background()

	// Exhaust app-a's burst
	for i := 0; i < 2; i++ {
		registry.EvaluateLimit(ctx, "app-a", 1)
	}

	// app-a should be rejected
	allowed, _ := registry.EvaluateLimit(ctx, "app-a", 1)
	if allowed {
		t.Error("app-a should be rate limited after exhausting burst")
	}

	// app-b should still be allowed (independent limiter)
	allowed, _ = registry.EvaluateLimit(ctx, "app-b", 1)
	if !allowed {
		t.Error("app-b should still be allowed; rate limiting one app should not affect another")
	}
}

func TestRateLimiter_UnknownApp_AllowsByDefault(t *testing.T) {
	// An app with no configured limiter should be allowed by default.

	registry := NewRateLimiterRegistry()
	ctx := context.Background()

	allowed, delay := registry.EvaluateLimit(ctx, "app-unknown", 1)
	if !allowed {
		t.Error("requests for apps without a configured limiter should be allowed by default")
	}
	if delay != 0 {
		t.Errorf("requests for unconfigured apps should have zero delay, got %v", delay)
	}
}
