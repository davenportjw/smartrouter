package limiter

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiterRegistry(t *testing.T) {
	registry := NewRateLimiterRegistry()

	t.Run("Add and Retrieve Limiter", func(t *testing.T) {
		registry.UpdateLimiter("client-1", 60, 1000, "high", false)

		registry.mu.RLock()
		limiter, exists := registry.limiters["client-1"]
		registry.mu.RUnlock()

		if !exists {
			t.Fatalf("limiter was not added to registry")
		}
		if limiter.rpm != 60 || limiter.tpm != 1000 || limiter.priority != "high" || limiter.optOutTPM {
			t.Errorf("limiter configured incorrectly: %+v", limiter)
		}
	})

	t.Run("Update Existing Limiter (Identical settings should not recreate)", func(t *testing.T) {
		registry.UpdateLimiter("client-1", 60, 1000, "high", false)
		// Retrieve limiter instance pointer
		registry.mu.RLock()
		ptr1 := registry.limiters["client-1"]
		registry.mu.RUnlock()

		// Update with same settings
		registry.UpdateLimiter("client-1", 60, 1000, "high", false)
		registry.mu.RLock()
		ptr2 := registry.limiters["client-1"]
		registry.mu.RUnlock()

		if ptr1 != ptr2 {
			t.Errorf("limiter recreated for identical settings")
		}

		// Update with different settings
		registry.UpdateLimiter("client-1", 120, 2000, "medium", true)
		registry.mu.RLock()
		ptr3 := registry.limiters["client-1"]
		registry.mu.RUnlock()

		if ptr1 == ptr3 {
			t.Errorf("limiter was not recreated for new settings")
		}
		if ptr3.rpm != 120 || ptr3.tpm != 2000 || ptr3.priority != "medium" || !ptr3.optOutTPM {
			t.Errorf("limiter updated incorrectly: %+v", ptr3)
		}
	})

	t.Run("AdjustLimiter", func(t *testing.T) {
		// Reset limiter
		registry.UpdateLimiter("client-adjust", 60, 1000, "standard", false)
		
		// Verify AdjustLimiter consumes tokens without failing
		registry.AdjustLimiter("client-adjust", 100)
		// Verify nonexistent client adjustment does not panic
		registry.AdjustLimiter("client-nonexistent", 50)
	})

	t.Run("RemoveLimiter", func(t *testing.T) {
		registry.RemoveLimiter("client-1")
		registry.mu.RLock()
		_, exists := registry.limiters["client-1"]
		registry.mu.RUnlock()

		if exists {
			t.Errorf("limiter was not removed from registry")
		}
	})

	t.Run("EvaluateLimit Allowed Flows", func(t *testing.T) {
		ctx := context.Background()
		// Registry with unconfigured client allows by default
		allowed, delay := registry.EvaluateLimit(ctx, "client-new", 100)
		if !allowed || delay != 0 {
			t.Errorf("expected default allow for unconfigured client, got allowed=%t, delay=%v", allowed, delay)
		}

		// Configure a high limit
		registry.UpdateLimiter("client-allow", 6000, 100000, "low", false)
		allowed, delay = registry.EvaluateLimit(ctx, "client-allow", 10)
		if !allowed || delay != 0 {
			t.Errorf("expected allow for high limit client, got allowed=%t, delay=%v", allowed, delay)
		}
	})

	t.Run("EvaluateLimit Low Priority immediate rejection", func(t *testing.T) {
		ctx := context.Background()
		// RPM = 1 per minute (which is 1/60 limit per second)
		registry.UpdateLimiter("client-low", 1, 1000, "low", false)

		// First request should consume the burst capacity of 1
		allowed1, _ := registry.EvaluateLimit(ctx, "client-low", 1)
		if !allowed1 {
			t.Errorf("expected first request to be allowed")
		}

		// Second request should be rejected immediately
		allowed2, delay := registry.EvaluateLimit(ctx, "client-low", 1)
		if allowed2 || delay != 0 {
			t.Errorf("expected second request to be blocked immediately, got allowed=%t, delay=%v", allowed2, delay)
		}
	})

	t.Run("EvaluateLimit High and Medium Priority delaying", func(t *testing.T) {
		ctx := context.Background()

		// High priority: allows queueing up to 5s
		// RPM = 60 (1/sec), TPM = 120 (2/sec limit, burst = 120)
		registry.UpdateLimiter("client-high", 60, 120, "high", false)

		// Consume TPM burst capacity completely (120 tokens)
		allowed1, _ := registry.EvaluateLimit(ctx, "client-high", 120)
		if !allowed1 {
			t.Errorf("expected first request to consume burst capacity")
		}

		// Second request requires TPM reservation. Delay will be 1 token / 2 tokens/sec = 0.5s.
		allowed2, delay := registry.EvaluateLimit(ctx, "client-high", 1)
		if !allowed2 {
			t.Errorf("expected high priority request to be allowed with delay, but was rejected")
		}
		if delay <= 0 {
			t.Errorf("expected high priority delay to be > 0, got %v", delay)
		}

		// Medium priority: allows queueing up to 2s
		// Delay will be 1 token / 2 tokens/sec = 0.5s.
		registry.UpdateLimiter("client-medium", 60, 120, "medium", false)

		// Consume TPM burst capacity completely (120 tokens)
		allowedMed1, _ := registry.EvaluateLimit(ctx, "client-medium", 120)
		if !allowedMed1 {
			t.Errorf("expected first request to consume burst capacity")
		}

		// Second request requires TPM reservation
		allowedMed2, delayMed := registry.EvaluateLimit(ctx, "client-medium", 1)
		if !allowedMed2 {
			t.Errorf("expected medium priority request to be allowed with delay, but was rejected")
		}
		if delayMed <= 0 {
			t.Errorf("expected medium priority delay to be > 0, got %v", delayMed)
		}
	})

	t.Run("EvaluateLimit does not leak RPM token when TPM fails", func(t *testing.T) {
		ctx := context.Background()
		// RPM = 1 (burst 1), TPM = 100 (burst 100), low priority
		registry.UpdateLimiter("client-noleak", 1, 100, "low", false)

		// Request 200 tokens (> burst 100): TPM must fail and RPM reservation must be cancelled
		allowed1, _ := registry.EvaluateLimit(ctx, "client-noleak", 200)
		if allowed1 {
			t.Fatalf("expected request exceeding TPM burst to be rejected")
		}

		// Subsequent request with 1 token must succeed immediately because RPM token was not leaked
		allowed2, delay2 := registry.EvaluateLimit(ctx, "client-noleak", 1)
		if !allowed2 || delay2 != 0 {
			t.Fatalf("expected subsequent request with 1 token to succeed immediately without RPM leak, got allowed=%t, delay=%v", allowed2, delay2)
		}
	})

	t.Run("EvaluateLimit High Priority does not double-consume RPM tokens on TPM delay", func(t *testing.T) {
		ctx := context.Background()
		// RPM = 2 (burst 2), TPM = 600 (10 tokens/sec, burst 600), high priority
		registry.UpdateLimiter("client-nodouble", 2, 600, "high", false)

		// Request 1: consumes 1 RPM token (1 left) and all 600 TPM tokens (0 left)
		allowed1, delay1 := registry.EvaluateLimit(ctx, "client-nodouble", 600)
		if !allowed1 || delay1 != 0 {
			t.Fatalf("expected first request to succeed immediately, got allowed=%t, delay=%v", allowed1, delay1)
		}

		// Request 2: requests 1 TPM token (delay = 0.1s). Should consume the 2nd RPM token (not double-consume RPM!).
		allowed2, delay2 := registry.EvaluateLimit(ctx, "client-nodouble", 1)
		if !allowed2 || delay2 <= 0 || delay2 > 500*time.Millisecond {
			t.Fatalf("expected second request to wait ~100ms for 1 TPM token without RPM delay, got allowed=%t, delay=%v", allowed2, delay2)
		}
	})

	t.Run("EvaluateLimit cancels reservations on context cancellation", func(t *testing.T) {
		// RPM = 60, TPM = 60 (1 token/sec, burst 60), high priority
		registry.UpdateLimiter("client-ctxcancel", 60, 60, "high", false)

		// Exhaust TPM burst
		allowed1, _ := registry.EvaluateLimit(context.Background(), "client-ctxcancel", 60)
		if !allowed1 {
			t.Fatalf("expected initial burst request to succeed")
		}

		// Next request needs 2 tokens (2s wait), but context cancels after 20ms
		cancelCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		allowed2, _ := registry.EvaluateLimit(cancelCtx, "client-ctxcancel", 2)
		if allowed2 {
			t.Fatalf("expected request with cancelled context to be rejected")
		}
	})

	t.Run("AdjustLimiter chunked positive overage and negative refund", func(t *testing.T) {
		ctx := context.Background()
		// RPM = 100, TPM = 100 (burst 100), low priority
		registry.UpdateLimiter("client-chunk-adjust", 100, 100, "low", false)

		// Consume 50 tokens -> 50 left
		allowed1, _ := registry.EvaluateLimit(ctx, "client-chunk-adjust", 50)
		if !allowed1 {
			t.Fatalf("expected 50-token request to succeed")
		}

		// Refund 40 tokens (negative correction) -> 90 left
		registry.AdjustLimiter("client-chunk-adjust", -40)

		// Now a request for 80 tokens should succeed (since 90 >= 80)
		allowed2, _ := registry.EvaluateLimit(ctx, "client-chunk-adjust", 80)
		if !allowed2 {
			t.Fatalf("expected 80-token request to succeed after refund")
		}

		// Now apply a large positive correction exceeding burst (e.g., +250 tokens when burst is 100)
		registry.AdjustLimiter("client-chunk-adjust", 250)

		// Subsequent request for 1 token must be rejected because bucket is in deep deficit
		allowed3, _ := registry.EvaluateLimit(ctx, "client-chunk-adjust", 1)
		if allowed3 {
			t.Fatalf("expected request after chunked positive token correction > burst to be rejected")
		}
	})
}

