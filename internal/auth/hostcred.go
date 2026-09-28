package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// HostCredentialPrefix marks a host credential so a leaked value is
// recognisable in logs and secret scanners.
const HostCredentialPrefix = "hc_"

// GenerateHostCredential issues the long-lived secret a host agent presents on
// every reconnect after its first enrolment. Only the SHA-256 hash is stored;
// the raw value is sent to the agent once and persisted on the host.
//
// Registration tokens are single-use, so without this credential an agent that
// restarted — or merely lost its connection — could never rejoin.
func GenerateHostCredential() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("auth: failed to generate host credential: %w", err)
	}
	raw = HostCredentialPrefix + hex.EncodeToString(b)
	return raw, HashAPIKey(raw), nil
}

// ValidHostCredentialFormat reports whether s looks like a host credential.
func ValidHostCredentialFormat(s string) bool {
	return strings.HasPrefix(s, HostCredentialPrefix) && len(s) == len(HostCredentialPrefix)+64
}
