package limiter

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ClientLimiter manages rate limits (both RPM and TPM) for a specific client.
type ClientLimiter struct {
	rpmLimiter *rate.Limiter
	tpmLimiter *rate.Limiter
	priority   string // "high", "medium", "low"
}

// NewClientLimiter creates a rate limiter for a client based on RPM and TPM settings.
func NewClientLimiter(rpm, tpm int, priority string) *ClientLimiter {
	// Default to reasonable limits if not configured
	if rpm <= 0 {
		rpm = 60
	}
	if tpm <= 0 {
		tpm = 40000
	}

	return &ClientLimiter{
		// Limit per second = limit / 60
		rpmLimiter: rate.NewLimiter(rate.Limit(float64(rpm)/60.0), rpm),
		tpmLimiter: rate.NewLimiter(rate.Limit(float64(tpm)/60.0), tpm),
		priority:   priority,
	}
}

// RateLimiterRegistry manages all client limiters dynamically.
type RateLimiterRegistry struct {
	mu       sync.RWMutex
	limiters map[string]*ClientLimiter
}

// NewRateLimiterRegistry initializes an empty rate limiting registry.
func NewRateLimiterRegistry() *RateLimiterRegistry {
	return &RateLimiterRegistry{
		limiters: make(map[string]*ClientLimiter),
	}
}

// UpdateLimiter dynamically updates or adds a client rate limiter.
func (rl *RateLimiterRegistry) UpdateLimiter(clientID string, rpm, tpm int, priority string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.limiters[clientID] = NewClientLimiter(rpm, tpm, priority)
}

// RemoveLimiter removes a client rate limiter on deletion.
func (rl *RateLimiterRegistry) RemoveLimiter(clientID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.limiters, clientID)
}

// EvaluateLimit checks rate limits and applies priority-based delaying for lower priority requests under load.
func (rl *RateLimiterRegistry) EvaluateLimit(ctx context.Context, clientID string, tokensRequested int) (bool, time.Duration) {
	rl.mu.RLock()
	limiter, exists := rl.limiters[clientID]
	rl.mu.RUnlock()

	if !exists {
		// If client has no limits configured, allow by default
		return true, 0
	}

	// Check RPM (1 request token)
	rpmAllowed := limiter.rpmLimiter.Allow()
	// Check TPM (tokensRequested tokens)
	tpmAllowed := limiter.tpmLimiter.AllowN(time.Now(), tokensRequested)

	if rpmAllowed && tpmAllowed {
		return true, 0
	}

	// Priority-based buffering: if rate limits are exceeded, instead of immediate rejection,
	// we can delay/queue the request if it has higher priority, or reject immediately if low priority.
	if limiter.priority == "high" {
		// High-priority requests can wait/queue up to 5 seconds to get a token instead of failing immediately
		reservationRPM := limiter.rpmLimiter.Reserve()
		reservationTPM := limiter.tpmLimiter.ReserveN(time.Now(), tokensRequested)

		delayRPM := reservationRPM.Delay()
		delayTPM := reservationTPM.Delay()

		maxDelay := delayRPM
		if delayTPM > maxDelay {
			maxDelay = delayTPM
		}

		if maxDelay > 0 && maxDelay < 5*time.Second {
			// Allow the request but tell the proxy to sleep for the delay duration before executing
			return true, maxDelay
		}

		// Cancel reservations if delay is too long to avoid starvation
		reservationRPM.Cancel()
		reservationTPM.Cancel()
	}

	// Medium-priority requests can wait up to 2 seconds
	if limiter.priority == "medium" {
		reservationRPM := limiter.rpmLimiter.Reserve()
		reservationTPM := limiter.tpmLimiter.ReserveN(time.Now(), tokensRequested)

		delayRPM := reservationRPM.Delay()
		delayTPM := reservationTPM.Delay()

		maxDelay := delayRPM
		if delayTPM > maxDelay {
			maxDelay = delayTPM
		}

		if maxDelay > 0 && maxDelay < 2*time.Second {
			return true, maxDelay
		}

		reservationRPM.Cancel()
		reservationTPM.Cancel()
	}

	// Low-priority requests are rejected immediately on rate limit breach (no queueing)
	return false, 0
}
