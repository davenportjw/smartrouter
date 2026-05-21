package limiter

import (
	"context"
	"testing"
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
}
