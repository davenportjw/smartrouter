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
	rpm        int
	tpm        int
	optOutTPM  bool
}

// NewClientLimiter creates a rate limiter for a client based on RPM and TPM settings.
func NewClientLimiter(rpm, tpm int, priority string, optOutTPM bool) *ClientLimiter {
	// Default to reasonable limits if not configured
	if rpm <= 0 {
		rpm = 60
	}
	if tpm <= 0 {
		tpm = 40000
	}

	var tpmLimiter *rate.Limiter
	if !optOutTPM {
		tpmLimiter = rate.NewLimiter(rate.Limit(float64(tpm)/60.0), tpm)
	}

	return &ClientLimiter{
		// Limit per second = limit / 60
		rpmLimiter: rate.NewLimiter(rate.Limit(float64(rpm)/60.0), rpm),
		tpmLimiter: tpmLimiter,
		priority:   priority,
		rpm:        rpm,
		tpm:        tpm,
		optOutTPM:  optOutTPM,
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
func (rl *RateLimiterRegistry) UpdateLimiter(clientID string, rpm, tpm int, priority string, optOutTPM bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// If limiter already exists with identical settings, do not reset it
	if existing, exists := rl.limiters[clientID]; exists {
		if existing.rpm == rpm && existing.tpm == tpm && existing.priority == priority && existing.optOutTPM == optOutTPM {
			return
		}
	}

	rl.limiters[clientID] = NewClientLimiter(rpm, tpm, priority, optOutTPM)
}

// AdjustLimiter dynamically corrects the token count balance.
func (rl *RateLimiterRegistry) AdjustLimiter(clientID string, tokenCorrection int) {
	if tokenCorrection == 0 {
		return
	}

	rl.mu.RLock()
	limiter, exists := rl.limiters[clientID]
	rl.mu.RUnlock()

	if !exists || limiter.optOutTPM || limiter.tpmLimiter == nil {
		return
	}

	now := time.Now()
	if tokenCorrection < 0 {
		// Refund tokens without exceeding the configured burst capacity
		current := limiter.tpmLimiter.TokensAt(now)
		headroom := int(float64(limiter.tpm) - current)
		if headroom <= 0 {
			return
		}
		refund := -tokenCorrection
		if refund > headroom {
			refund = headroom
		}
		_ = limiter.tpmLimiter.AllowN(now, -refund)
		return
	}

	// Positive correction: deduct extra tokens in chunks of at most burst (limiter.tpm)
	remaining := tokenCorrection
	burst := limiter.tpm
	if burst <= 0 {
		burst = 1
	}
	for remaining > 0 {
		chunk := remaining
		if chunk > burst {
			chunk = burst
		}
		_ = limiter.tpmLimiter.ReserveN(now, chunk)
		remaining -= chunk
	}
}

// RemoveLimiter removes a client rate limiter on deletion.
func (rl *RateLimiterRegistry) RemoveLimiter(clientID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.limiters, clientID)
}

// EvaluateLimit checks rate limits and applies priority-based delaying for higher priority requests under load.
func (rl *RateLimiterRegistry) EvaluateLimit(ctx context.Context, clientID string, tokensRequested int) (bool, time.Duration) {
	rl.mu.RLock()
	limiter, exists := rl.limiters[clientID]
	rl.mu.RUnlock()

	if !exists {
		// If client has no limits configured, allow by default
		return true, 0
	}

	now := time.Now()
	resRPM := limiter.rpmLimiter.ReserveN(now, 1)
	if !resRPM.OK() {
		return false, 0
	}

	var resTPM *rate.Reservation
	if !limiter.optOutTPM && limiter.tpmLimiter != nil {
		if tokensRequested <= 0 {
			tokensRequested = 1
		}
		resTPM = limiter.tpmLimiter.ReserveN(now, tokensRequested)
		if !resTPM.OK() {
			resRPM.CancelAt(now)
			return false, 0
		}
	}

	maxDelay := resRPM.DelayFrom(now)
	if resTPM != nil {
		if delayTPM := resTPM.DelayFrom(now); delayTPM > maxDelay {
			maxDelay = delayTPM
		}
	}

	if maxDelay <= 0 {
		return true, 0
	}

	var threshold time.Duration
	switch limiter.priority {
	case "high":
		threshold = 5 * time.Second
	case "medium":
		threshold = 2 * time.Second
	default:
		threshold = 0
	}

	if threshold <= 0 || maxDelay >= threshold {
		resRPM.CancelAt(now)
		if resTPM != nil {
			resTPM.CancelAt(now)
		}
		return false, 0
	}

	select {
	case <-ctx.Done():
		cancelNow := time.Now()
		resRPM.CancelAt(cancelNow)
		if resTPM != nil {
			resTPM.CancelAt(cancelNow)
		}
		return false, 0
	case <-time.After(maxDelay):
		return true, maxDelay
	}
}

