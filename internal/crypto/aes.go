// Package crypto provides column-level encryption for sensitive PII data.
// In accordance with DPDP compliance and security requirements,
// fields like PAN, bank details, phone numbers, and GSTIN are encrypted at rest
// using AES-256-GCM prior to database insertion.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var (
	ErrInvalidKeySize    = errors.New("crypto: key must be exactly 32 bytes for AES-256")
	ErrInvalidCiphertext = errors.New("crypto: ciphertext is too short or malformed")
)

// Cipher handles AES-256-GCM encryption and decryption.
type Cipher struct {
	key []byte
}

// NewCipher creates a new Cipher using a 32-byte secret key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKeySize
	}
	return &Cipher{key: key}, nil
}

// Encrypt encrypts plaintext using AES-256-GCM.
// Returns a byte slice containing [12-byte nonce || ciphertext + 16-byte tag].
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create cipher block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create GCM mode: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: failed to generate nonce: %w", err)
	}

	// Output format: nonce + ciphertext
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts ciphertext using AES-256-GCM.
// Expects input format: [12-byte nonce || ciphertext + 16-byte tag].
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, nil
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create cipher block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create GCM mode: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrInvalidCiphertext
	}

	nonce, encryptedMessage := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, encryptedMessage, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decryption failed: %w", err)
	}

	return plaintext, nil
}

// EncryptString helper for string values.
func (c *Cipher) EncryptString(plaintext string) ([]byte, error) {
	return c.Encrypt([]byte(plaintext))
}

// DecryptString helper for returning string values.
func (c *Cipher) DecryptString(ciphertext []byte) (string, error) {
	b, err := c.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
