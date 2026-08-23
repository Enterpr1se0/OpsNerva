package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Encryptor struct {
	aead cipher.AEAD
}

func NewEncryptor(encodedKey, dataDir string) (*Encryptor, error) {
	key, err := loadOrCreateKey(encodedKey, dataDir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Encryptor{aead: aead}, nil
}

func (e *Encryptor) Encrypt(plain []byte) (string, error) {
	if len(plain) == 0 {
		return "", nil
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := e.aead.Seal(nil, nonce, plain, nil)
	payload := append(nonce, sealed...)
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func (e *Encryptor) Decrypt(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(payload) < e.aead.NonceSize() {
		return nil, errors.New("encrypted payload is too short")
	}
	nonce := payload[:e.aead.NonceSize()]
	return e.aead.Open(nil, nonce, payload[e.aead.NonceSize():], nil)
}

func loadOrCreateKey(encodedKey, dataDir string) ([]byte, error) {
	if encodedKey != "" {
		key, err := decodeKey(encodedKey)
		if err != nil {
			return nil, fmt.Errorf("decode OPSNERVA_MASTER_KEY: %w", err)
		}
		return key, nil
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "master.key")
	data, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(strings.TrimSpace(string(data)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := base64.RawStdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func decodeKey(value string) ([]byte, error) {
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
