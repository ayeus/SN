package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrPasswordTooShort = errors.New("auth: password must be at least 8 characters long")
	ErrInvalidPassword = errors.New("auth: invalid email or password")
)

// HashPassword hashes a plain text password using bcrypt.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", ErrPasswordTooShort
	}

	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
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
