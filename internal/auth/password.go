package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrPasswordTooShort  = errors.New("auth: password must be at least 12 characters long")
	ErrPasswordTooLong   = errors.New("auth: password must be at most 72 bytes")
	ErrPasswordTooSimple = errors.New("auth: password must contain at least three of: lowercase, uppercase, digit, symbol")
	ErrPasswordCommon    = errors.New("auth: password is too common; choose something less predictable")
	ErrInvalidPassword   = errors.New("auth: invalid email or password")
)

// MinPasswordLength is the floor for new passwords. NIST SP 800-63B favours
// length over composition rules, but a short minimum with no other checks lets
// "password" through, so both are applied.
const MinPasswordLength = 12

// bcryptMaxInputBytes is a hard limit of the algorithm: bcrypt silently ignores
// input past 72 bytes, which would make two different long passwords equivalent.
const bcryptMaxInputBytes = 72

// commonPasswords are the values that dominate credential-stuffing lists. This
// is a floor, not a substitute for a breach-corpus check (e.g. HaveIBeenPwned's
// k-anonymity range API), which belongs behind an outbound HTTP call.
var commonPasswords = map[string]bool{
	"password":     true,
	"password1":    true,
	"password123":  true,
	"passw0rd":     true,
	"qwerty":       true,
	"qwerty123":    true,
	"123456":       true,
	"1234567890":   true,
	"111111":       true,
	"letmein":      true,
	"welcome":      true,
	"welcome123":   true,
	"admin":        true,
	"admin123":     true,
	"iloveyou":     true,
	"monkey":       true,
	"dragon":       true,
	"football":     true,
	"changeme":     true,
	"secret":       true,
	"abc123":       true,
	"p@ssw0rd":     true,
	"trustno1":     true,
	"sunshine":     true,
	"princess":     true,
	"master":       true,
	"login":        true,
	"starwars":     true,
	"whatever":     true,
	"ayeusann":     true,
	"ayeusann123":  true,
	"gpu123456789": true,
}

// ValidatePassword enforces the password policy. It is separate from
// HashPassword so callers can validate before doing expensive work.
func ValidatePassword(password string) error {
	if len(password) > bcryptMaxInputBytes {
		return ErrPasswordTooLong
	}
	// The corpus check runs before the length check so that a known-bad password
	// gets the specific error rather than a generic "too short".
	if commonPasswords[strings.ToLower(strings.TrimSpace(password))] {
		return ErrPasswordCommon
	}
	if len([]rune(password)) < MinPasswordLength {
		return ErrPasswordTooShort
	}

	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range password {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasSymbol = true
		}
	}

	classes := 0
	for _, ok := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if ok {
			classes++
		}
	}
	if classes < 3 {
		return ErrPasswordTooSimple
	}

	return nil
}

// HashPassword validates and hashes a plain text password using bcrypt.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost())
	if err != nil {
		return "", fmt.Errorf("auth: failed to hash password: %w", err)
	}

	return string(bytes), nil
}

// CheckPasswordHash verifies a plain text password against a bcrypt hash.
func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// dummyHash is a valid bcrypt hash of a random value, used to spend the same CPU
// time on a login for an address that does not exist.
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.aQqQY9CrPPCa7gnRuTMH6BcQyC1Kt1O"

// DummyPasswordCheck performs a bcrypt comparison against a fixed hash so that
// authentication failures take comparable time whether or not the account
// exists. Without this, response latency reveals which emails are registered.
func DummyPasswordCheck() {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte("timing-equalizer"))
}

// NeedsRehash reports whether a stored hash was produced with a weaker cost than
// the current setting, so it can be upgraded on the user's next successful login.
func NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return true
	}
	return cost < bcryptCost()
}

// bcryptCost returns the work factor. 12 is a reasonable 2026 default: roughly
// 250ms on server hardware, which is tolerable for login and expensive to
// brute-force offline.
func bcryptCost() int {
	return 12
}
