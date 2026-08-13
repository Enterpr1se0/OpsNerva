package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	portableSchema        = "opsnerva.configuration.encrypted"
	portableSchemaVersion = 1
	portableKDFTime       = 3
	portableKDFMemoryKiB  = 64 * 1024
	portableKDFThreads    = 1
)

var ErrPortablePassword = errors.New("invalid migration package password or corrupted package")

type portableEnvelope struct {
	Schema        string         `json:"schema"`
	SchemaVersion int            `json:"schema_version"`
	KDF           portableKDF    `json:"kdf"`
	Cipher        portableCipher `json:"cipher"`
	Ciphertext    string         `json:"ciphertext"`
}

type portableKDF struct {
	Name      string `json:"name"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

type portableCipher struct {
	Name  string `json:"name"`
	Nonce string `json:"nonce"`
}

func SealPortable(password string, plaintext []byte) ([]byte, error) {
	if password == "" {
		return nil, fmt.Errorf("migration package password is required")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate migration salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, portableKDFTime, portableKDFMemoryKiB, portableKDFThreads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate migration nonce: %w", err)
	}
	aad := portableAssociatedData(portableSchemaVersion)
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	return json.MarshalIndent(portableEnvelope{
		Schema: portableSchema, SchemaVersion: portableSchemaVersion,
		KDF:        portableKDF{Name: "argon2id", Salt: base64.RawURLEncoding.EncodeToString(salt), Time: portableKDFTime, MemoryKiB: portableKDFMemoryKiB, Threads: portableKDFThreads},
		Cipher:     portableCipher{Name: "aes-256-gcm", Nonce: base64.RawURLEncoding.EncodeToString(nonce)},
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}, "", "  ")
}

func OpenPortable(password string, encoded []byte) ([]byte, bool, error) {
	var envelope portableEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil || envelope.Schema != portableSchema {
		return nil, false, nil
	}
	if password == "" {
		return nil, true, fmt.Errorf("migration package password is required")
	}
	if envelope.SchemaVersion != portableSchemaVersion || envelope.KDF.Name != "argon2id" || envelope.Cipher.Name != "aes-256-gcm" {
		return nil, true, fmt.Errorf("unsupported encrypted migration package")
	}
	if envelope.KDF.Time != portableKDFTime || envelope.KDF.MemoryKiB != portableKDFMemoryKiB || envelope.KDF.Threads != portableKDFThreads {
		return nil, true, fmt.Errorf("unsupported migration key derivation parameters")
	}
	salt, err := base64.RawURLEncoding.DecodeString(envelope.KDF.Salt)
	if err != nil || len(salt) != 16 {
		return nil, true, ErrPortablePassword
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Cipher.Nonce)
	if err != nil {
		return nil, true, ErrPortablePassword
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, true, ErrPortablePassword
	}
	key := argon2.IDKey([]byte(password), salt, envelope.KDF.Time, envelope.KDF.MemoryKiB, envelope.KDF.Threads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, true, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, true, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, true, ErrPortablePassword
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, portableAssociatedData(envelope.SchemaVersion))
	if err != nil {
		return nil, true, ErrPortablePassword
	}
	return plaintext, true, nil
}

func IsPortableEncrypted(encoded []byte) bool {
	var header struct {
		Schema string `json:"schema"`
	}
	return json.Unmarshal(encoded, &header) == nil && header.Schema == portableSchema
}

func portableAssociatedData(version int) []byte {
	return []byte(fmt.Sprintf("%s:v%d", portableSchema, version))
}
