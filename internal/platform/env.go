package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// knownWeakSecrets are placeholder values that must never reach production.
// They exist in .env.example and in old source defaults; RequireSecret rejects them.
var knownWeakSecrets = map[string]bool{
	"dev-secret-do-not-use-in-production":         true,
	"dev-secret-key-32-bytes-long-super-secure!":  true,
	"dev-only-insecure-jwt-signing-key-0001":      true,
	"dev-only-insecure-internal-service-key-0001": true,
	"changeme": true,
	"secret":   true,
	"password": true,
}

// IsProduction reports whether the service is running outside a developer machine.
// Anything other than SN_ENV=dev or SN_ENV=test is treated as production, so a
// missing or misspelled SN_ENV fails closed rather than silently relaxing checks.
func IsProduction() bool {
	switch strings.ToLower(os.Getenv("SN_ENV")) {
	case "dev", "development", "test":
		return false
	default:
		return true
	}
}

// Env reads an environment variable, falling back to defaultVal when unset or empty.
// Use this only for non-sensitive settings such as ports and log levels.
func Env(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// EnvInt reads an integer environment variable, falling back to defaultVal when
// unset or unparseable.
func EnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

// EnvBool reads a boolean environment variable, falling back to defaultVal when
// unset or unparseable.
func EnvBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

// RequireEnv reads a required environment variable. In production an unset or
// empty value is fatal. In dev and test the devDefault is used so that local
// workflows keep working without a full .env file.
//
// This replaces the old MustEnv, which never actually required anything.
func RequireEnv(key, devDefault string) (string, error) {
	v := os.Getenv(key)
	if v != "" {
		return v, nil
	}
	if IsProduction() {
		return "", fmt.Errorf("platform: required environment variable %s is not set (SN_ENV=%s)", key, os.Getenv("SN_ENV"))
	}
	if devDefault == "" {
		return "", fmt.Errorf("platform: required environment variable %s is not set and has no dev default", key)
	}
	return devDefault, nil
}

// RequireSecret reads a required secret. On top of RequireEnv's presence check it
// enforces a minimum length and rejects known placeholder values, so a service
// cannot start in production with the dev JWT signing key.
func RequireSecret(key, devDefault string, minLen int) (string, error) {
	v, err := RequireEnv(key, devDefault)
	if err != nil {
		return "", err
	}

	if !IsProduction() {
		return v, nil
	}

	if knownWeakSecrets[v] {
		return "", fmt.Errorf("platform: %s is set to a known placeholder value; generate a real secret before starting in production", key)
	}
	if len(v) < minLen {
		return "", fmt.Errorf("platform: %s must be at least %d characters in production (got %d)", key, minLen, len(v))
	}
	return v, nil
}

// MustEnv reads an environment variable or returns a default value.
//
// Deprecated: MustEnv does not require anything despite its name, which let
// services boot in production with dev defaults. Use Env for non-sensitive
// settings, RequireEnv for required config, and RequireSecret for secrets.
func MustEnv(key, defaultVal string) string {
	return Env(key, defaultVal)
}
