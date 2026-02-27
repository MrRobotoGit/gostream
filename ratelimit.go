package main

import (
	"context"
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter using a semaphore pattern
// with automatic reset every interval to prevent starvation
type RateLimiter struct {
	sem      chan struct{} // Buffered channel acts as token bucket
	capacity int           // Maximum tokens (requests per interval)
	interval time.Duration // Reset interval (default: 1 second)
	mu       sync.Mutex    // Protects reset operations
	stopCh   chan struct{} // Signal to stop reset goroutine
	stopped  bool          // Tracks if limiter has been stopped
}

// NewRateLimiter creates a new rate limiter with specified capacity and interval
// Example: NewRateLimiter(100, 1*time.Second) = 100 requests per second
func NewRateLimiter(capacity int, interval time.Duration) *RateLimiter {
	rl := &RateLimiter{
		sem:      make(chan struct{}, capacity),
		capacity: capacity,
		interval: interval,
		stopCh:   make(chan struct{}),
		stopped:  false,
	}

	// Fill initial tokens
	for i := 0; i < capacity; i++ {
		rl.sem <- struct{}{}
	}

	// Start background reset goroutine
	go rl.resetLoop()

	return rl
}

// Acquire attempts to acquire a token for rate limiting
// Blocks until a token is available or context is cancelled
// Returns error if context is cancelled before acquiring token
func (rl *RateLimiter) Acquire(ctx context.Context) error {
	select {
	case <-rl.sem:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resetLoop runs in background and resets token bucket every interval
// This ensures rate limit applies per-interval rather than globally
func (rl *RateLimiter) resetLoop() {
	ticker := time.NewTicker(rl.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.reset()
		case <-rl.stopCh:
			return
		}
	}
}

// reset refills the token bucket to capacity
// Drains remaining tokens and refills to ensure clean reset
func (rl *RateLimiter) reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.stopped {
		return
	}

	// Drain remaining tokens
	for {
		select {
		case <-rl.sem:
			// Drained one token
		default:
			// Channel empty, stop draining
			goto refill
		}
	}

refill:
	// Refill to capacity
	for i := 0; i < rl.capacity; i++ {
		select {
		case rl.sem <- struct{}{}:
			// Token added
		default:
			// Should not happen after drain, but safety check
			return
		}
	}
}

// Stop stops the rate limiter and cleans up resources
// Should be called when rate limiter is no longer needed
func (rl *RateLimiter) Stop() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if !rl.stopped {
		rl.stopped = true
		close(rl.stopCh)
	}
}
