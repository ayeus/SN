package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter implements a Redis-based sliding window rate limiter.
// Each API key is limited to a configurable number of requests per minute.
type RateLimiter struct {
	rdb        *redis.Client
	defaultRPM int
}

// NewRateLimiter creates a new Redis-backed rate limiter.
func NewRateLimiter(rdb *redis.Client, defaultRPM int) *RateLimiter {
	if defaultRPM <= 0 {
		defaultRPM = 60
	}
	return &RateLimiter{
		rdb:        rdb,
		defaultRPM: defaultRPM,
	}
}

// RateLimitResult holds the result of a rate limit check.
type RateLimitResult struct {
	Allowed    bool
	Remaining  int
	ResetAt    time.Time
	RetryAfter time.Duration
}

// Check verifies whether the given API key is within its rate limit.
// Uses a Redis sorted set as a sliding window.
func (rl *RateLimiter) Check(ctx context.Context, apiKeyID string) (*RateLimitResult, error) {
	now := time.Now()
	windowStart := now.Add(-1 * time.Minute)
	key := fmt.Sprintf("ratelimit:%s", apiKeyID)

	pipe := rl.rdb.Pipeline()

	// Remove entries older than the window
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", windowStart.UnixNano()))

	// Count current entries in the window
	countCmd := pipe.ZCard(ctx, key)

	// Add current request timestamp
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(now.UnixNano()),
		Member: fmt.Sprintf("%d", now.UnixNano()),
	})

	// Set TTL on the key (2 minutes to be safe)
	pipe.Expire(ctx, key, 2*time.Minute)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("rate_limiter: redis pipeline error: %w", err)
	}

	currentCount := int(countCmd.Val())
	remaining := rl.defaultRPM - currentCount - 1 // -1 for the request we just added
	if remaining < 0 {
		remaining = 0
	}

	result := &RateLimitResult{
		Allowed:   currentCount < rl.defaultRPM,
		Remaining: remaining,
		ResetAt:   now.Add(1 * time.Minute),
	}

	if !result.Allowed {
		// Remove the request we just added since it's not allowed
		rl.rdb.ZRem(ctx, key, fmt.Sprintf("%d", now.UnixNano()))
		result.RetryAfter = time.Until(result.ResetAt)
		if result.RetryAfter < time.Second {
			result.RetryAfter = time.Second
		}
	}

	return result, nil
}

// WriteRateLimitHeaders sets standard rate limit headers on the response.
func (rl *RateLimiter) WriteRateLimitHeaders(w http.ResponseWriter, result *RateLimitResult) {
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.defaultRPM))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(result.ResetAt.Unix(), 10))

	if !result.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
	}
}

// NullRateLimiter is a no-op rate limiter used when Redis is unavailable.
type NullRateLimiter struct{}

// Check always allows requests when Redis is unavailable.
func (n *NullRateLimiter) Check(_ context.Context, _ string) (*RateLimitResult, error) {
	return &RateLimitResult{
		Allowed:   true,
		Remaining: 999,
		ResetAt:   time.Now().Add(1 * time.Minute),
	}, nil
}
