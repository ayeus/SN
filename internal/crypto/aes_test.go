package crypto_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/ayeus/ayeusann/internal/crypto"
)

func TestAES256GCM(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher error: %v", err)
	}

	t.Run("EncryptDecryptBytes", func(t *testing.T) {
		original := []byte("ABCDE1234F") // Sample PAN
		encrypted, err := c.Encrypt(original)
		if err != nil {
			t.Fatalf("Encrypt error: %v", err)
		}
		if bytes.Equal(encrypted, original) {
			t.Fatal("Encrypted bytes match original bytes")
		}

		decrypted, err := c.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("Decrypt error: %v", err)
		}
		if !bytes.Equal(decrypted, original) {
			t.Fatalf("Expected %s, got %s", original, decrypted)
		}
	})

	t.Run("EncryptDecryptString", func(t *testing.T) {
		original := "+919876543210"
		encrypted, err := c.EncryptString(original)
		if err != nil {
			t.Fatalf("EncryptString error: %v", err)
		}

		decrypted, err := c.DecryptString(encrypted)
		if err != nil {
			t.Fatalf("DecryptString error: %v", err)
		}
		if decrypted != original {
			t.Fatalf("Expected %s, got %s", original, decrypted)
		}
	})

	t.Run("InvalidKeySize", func(t *testing.T) {
		shortKey := make([]byte, 16)
		_, err := crypto.NewCipher(shortKey)
		if err == nil {
			t.Fatal("Expected error for non-32-byte key")
		}
	})

	t.Run("TamperedCiphertext", func(t *testing.T) {
		encrypted, err := c.EncryptString("secret_bank_account")
		if err != nil {
			t.Fatalf("EncryptString error: %v", err)
		}

		// Tamper with ciphertext
		encrypted[len(encrypted)-1] ^= 0xFF

		_, err = c.DecryptString(encrypted)
		if err == nil {
			t.Fatal("Expected error when decrypting tampered ciphertext")
		}
	})
}
