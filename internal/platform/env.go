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

// RunMode is where an installation runs, chosen with SN_ENV.
type RunMode string

const (
	// ModeDev is a developer's checkout: published default secrets, addresses
	// taken from each request, simulated GPUs allowed.
	ModeDev RunMode = "dev"
	// ModeTest is the test suites.
	ModeTest RunMode = "test"
	// ModePrivate is a real installation for people who know each other, on a
	// network they trust (a home LAN or a private VPN). It keeps every
	// production safeguard except one: it may serve plain HTTP, because there
	// is no public name to hold a certificate.
	ModePrivate RunMode = "private"
	// ModeProduction is a public installation.
	ModeProduction RunMode = "production"
)

// Mode reads SN_ENV. Anything unrecognised, including an unset or misspelled
// value, is production, so a mistake fails closed rather than relaxing checks.
func Mode() RunMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SN_ENV"))) {
	case "dev", "development":
		return ModeDev
	case "test":
		return ModeTest
	case "private":
		return ModePrivate
	default:
		return ModeProduction
	}
}

// IsProduction reports whether this is a real installation (private or
// production) rather than a developer's machine or a test run. Real
// installations need real secrets and a signing key, and take nothing on trust
// from the request.
func IsProduction() bool {
	m := Mode()
	return m == ModePrivate || m == ModeProduction
}

// AllowsPlaintext reports whether a service may listen without TLS: always in
// development and on a private network, and in production only behind a proxy
// that terminates TLS.
func AllowsPlaintext() bool {
	return Mode() != ModeProduction || EnvBool("SN_TLS_TERMINATED_BY_PROXY", false)
}

// AllowsFakeGPU reports whether hosts reporting a simulated GPU may join. They
// exist for development and for end-to-end tests; on a real installation a
// simulated machine would be handed work it cannot do.
func AllowsFakeGPU() bool {
	return !IsProduction() || EnvBool("ALLOW_FAKE_GPU", false)
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
