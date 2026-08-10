package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	APIKeyPrefix = "sk_live_"
)

var (
	ErrInvalidAPIKeyFormat = errors.New("auth: invalid API key format, expected prefix 'sk_live_'")
)

// GenerateAPIKey generates a new random API key.
// Returns rawKey (e.g. "sk_live_a1b2c3d4..."), prefix (first 12 chars e.g. "sk_live_a1b2"), and hash (SHA-256 hex string).
func GenerateAPIKey() (rawKey string, prefix string, hash string, err error) {
	randomBytes := make([]byte, 24)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", "", fmt.Errorf("auth: failed to generate random bytes: %w", err)
	}

	randomHex := hex.EncodeToString(randomBytes)
	rawKey = APIKeyPrefix + randomHex
	prefix = rawKey[:12] // "sk_live_" (8 chars) + 4 hex chars = 12 chars
	hash = HashAPIKey(rawKey)

	return rawKey, prefix, hash, nil
}

// HashAPIKey computes the SHA-256 hex digest of a raw API key.
func HashAPIKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

// ExtractPrefix returns the prefix used for fast database indexing.
func ExtractPrefix(rawKey string) (string, error) {
	if len(rawKey) < 12 || rawKey[:8] != APIKeyPrefix {
		return "", ErrInvalidAPIKeyFormat
	}
	return rawKey[:12], nil
}
