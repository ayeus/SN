// Package config provides shared configuration helpers for AyeusANN services.
package config

import (
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/ayeus/ayeusann/internal/platform"
)

// NewRedisClient creates a Redis client from the REDIS_URL environment variable.
// Defaults to redis://localhost:6379 if not set.
func NewRedisClient() (*redis.Client, error) {
	redisURL := platform.MustEnv("REDIS_URL", "redis://localhost:6379")

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("config: failed to parse REDIS_URL: %w", err)
	}

	opts.PoolSize = 20
	opts.MinIdleConns = 5

	return redis.NewClient(opts), nil
}
