package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/awnumar/memguard"
)

// LoadMasterKey loads the master key from the environment and seals it into
// a memguard Enclave. The raw key bytes are wiped from regular memory
// immediately after sealing — the key only exists in encrypted form (Enclave)
// or in guarded, mlock'd memory (LockedBuffer) when actively needed.
func LoadMasterKey() (*memguard.Enclave, error) {
	keyHex := os.Getenv("AGENTLEDGER_MASTER_KEY")
	if keyHex == "" {
		return nil, errors.New("AGENTLEDGER_MASTER_KEY environment variable not set")
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid master key format: %w", err)
	}
	if len(key) != 32 {
		memguard.WipeBytes(key)
		return nil, fmt.Errorf("master key must be 32 bytes (got %d)", len(key))
	}

	// Seal the raw key into an encrypted Enclave and wipe the source.
	// NewEnclave copies the data and wipes src automatically.
	enclave := memguard.NewEnclave(key)
	return enclave, nil
}

// Encrypt encrypts a payload using AES-256-GCM. The master key is opened from
// its Enclave into a guarded LockedBuffer, used, then immediately destroyed.
func Encrypt(keyEnclave *memguard.Enclave, plaintext []byte) (string, error) {
	// Open key into mlock'd, guard-paged memory
	keyBuf, err := keyEnclave.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open master key enclave: %w", err)
	}
	defer keyBuf.Destroy()

	block, err := aes.NewCipher(keyBuf.Bytes())
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a hex-encoded cipher string using AES-256-GCM. The master key
// is opened from its Enclave into a guarded LockedBuffer, used, then destroyed.
// The returned plaintext is a regular []byte — the caller MUST call Zeroize()
// on it after use.
func Decrypt(keyEnclave *memguard.Enclave, encryptedHex string) ([]byte, error) {
	ciphertext, err := hex.DecodeString(encryptedHex)
	if err != nil {
		return nil, err
	}

	// Open key into mlock'd, guard-paged memory
	keyBuf, err := keyEnclave.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open master key enclave: %w", err)
	}
	defer keyBuf.Destroy()

	block, err := aes.NewCipher(keyBuf.Bytes())
	if err != nil {
		return nil, err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

// GenerateKey generates a random 32-byte key for encryption.
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Zeroize securely wipes a byte slice by overwriting it with zeroes.
// Delegates to memguard.WipeBytes for a hardened implementation.
func Zeroize(b []byte) {
	memguard.WipeBytes(b)
}
