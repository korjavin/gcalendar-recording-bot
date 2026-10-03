package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// connection is one connected Google Calendar, stored at
// DATA_DIR/connections/<emailHash(email)>.json.
type connection struct {
	Email        string    `json:"email"`
	RefreshToken []byte    `json:"refresh_token"` // AES-256-GCM: nonce || ciphertext, sealed with the e-mail as AAD
	ConnectedAt  time.Time `json:"connected_at"`
}

// emailHash names a connection file and stands in for the address in logs.
func emailHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(sum[:])
}

func connectionPath(dataDir, email string) string {
	return filepath.Join(dataDir, "connections", emailHash(email)+".json")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptToken seals plain under key with a fresh random nonce; aad binds the
// ciphertext to its owner so a token cannot be swapped into another file.
func encryptToken(key []byte, plain, aad string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), []byte(aad)), nil
}

func decryptToken(key, sealed []byte, aad string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("sealed token too short")
	}
	n := gcm.NonceSize()
	plain, err := gcm.Open(nil, sealed[:n], sealed[n:], []byte(aad))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// saveConnection encrypts the refresh token and atomically writes (or
// replaces) the owner's connection file with mode 0600.
func saveConnection(dataDir string, key []byte, email, refreshToken string, now time.Time) error {
	email = strings.ToLower(email)
	sealed, err := encryptToken(key, refreshToken, email)
	if err != nil {
		return err
	}
	data, err := json.Marshal(connection{Email: email, RefreshToken: sealed, ConnectedAt: now.UTC()})
	if err != nil {
		return err
	}
	path := connectionPath(dataDir, email)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
