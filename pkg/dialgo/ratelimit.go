package dialgo

import (
	"context"
	"sync"
	"time"
)

// SMSRateLimiter enforces per-minute rate limiting for SMS sends.
// Dialpad enforces 100 SMS/min (Tier 0) or 800 SMS/min (Tier 1).
// Using a sliding-window counter for accurate rate tracking.
type SMSRateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	requests []time.Time
}

// NewSMSRateLimiter creates a rate limiter with the given per-minute limit.
// If limit <= 0, rate limiting is disabled.
func NewSMSRateLimiter(perMinuteLimit int) *SMSRateLimiter {
	if perMinuteLimit <= 0 {
		return nil
	}
	return &SMSRateLimiter{
		limit:    perMinuteLimit,
		window:   time.Minute,
		requests: make([]time.Time, 0, perMinuteLimit),
	}
}

// Wait blocks until a send slot is available, respecting the rate limit.
// Returns error if context is cancelled while waiting.
func (rl *SMSRateLimiter) Wait(ctx context.Context) error {
	if rl == nil {
		return nil
	}

	for {
		rl.mu.Lock()
		now := time.Now()

		// Purge entries outside the window
		cutoff := now.Add(-rl.window)
		valid := 0
		for _, t := range rl.requests {
			if t.After(cutoff) {
				rl.requests[valid] = t
				valid++
			}
		}
		rl.requests = rl.requests[:valid]

		if len(rl.requests) < rl.limit {
			rl.requests = append(rl.requests, now)
			rl.mu.Unlock()
			return nil
		}

		// Earliest request in window — wait until it expires
		waitUntil := rl.requests[0].Add(rl.window)
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(waitUntil)):
			// Retry
		}
	}
}
