package security

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	configurationPackageMagic            = "OPSNERVA-CONFIG\x00"
	configurationPackageVersion          = byte(1)
	configurationPackageSaltSize         = 16
	configurationPackagePasswordMinRunes = 8
	configurationPackagePasswordMaxRunes = 1024
	configurationPackageScryptN          = 1 << 15
	configurationPackageScryptR          = 8
	configurationPackageScryptP          = 1
)

var ErrConfigurationPackageAuthentication = errors.New("configuration password is incorrect or the package is damaged")

func EncryptConfigurationPackage(plain []byte, password string) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("configuration package is empty")
	}
	if err := ValidateConfigurationPackagePassword(password); err != nil {
		return nil, err
	}
	salt := make([]byte, configurationPackageSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, key, err := configurationPackageAEAD(password, salt)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := make([]byte, 0, len(configurationPackageMagic)+1+len(salt)+len(nonce))
	header = append(header, configurationPackageMagic...)
	header = append(header, configurationPackageVersion)
	header = append(header, salt...)
	header = append(header, nonce...)
	return aead.Seal(header, nonce, plain, header), nil
}

func DecryptConfigurationPackage(payload []byte, password string) ([]byte, error) {
	if err := ValidateConfigurationPackagePassword(password); err != nil {
		return nil, err
	}
	headerSize := len(configurationPackageMagic) + 1 + configurationPackageSaltSize + 12
	if len(payload) < headerSize+16 || !bytes.Equal(payload[:len(configurationPackageMagic)], []byte(configurationPackageMagic)) {
		return nil, fmt.Errorf("invalid encrypted configuration package")
	}
	if payload[len(configurationPackageMagic)] != configurationPackageVersion {
		return nil, fmt.Errorf("unsupported encrypted configuration package version")
	}
	saltStart := len(configurationPackageMagic) + 1
	nonceStart := saltStart + configurationPackageSaltSize
	salt := payload[saltStart:nonceStart]
	aead, key, err := configurationPackageAEAD(password, salt)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	headerSize = nonceStart + aead.NonceSize()
	if len(payload) < headerSize+aead.Overhead() {
		return nil, fmt.Errorf("invalid encrypted configuration package")
	}
	header := payload[:headerSize]
	plain, err := aead.Open(nil, header[nonceStart:headerSize], payload[headerSize:], header)
	if err != nil {
		return nil, ErrConfigurationPackageAuthentication
	}
	return plain, nil
}

func ValidateConfigurationPackagePassword(password string) error {
	length := utf8.RuneCountInString(password)
	if !utf8.ValidString(password) || length < configurationPackagePasswordMinRunes || length > configurationPackagePasswordMaxRunes {
		return fmt.Errorf("configuration password must be between %d and %d characters", configurationPackagePasswordMinRunes, configurationPackagePasswordMaxRunes)
	}
	return nil
}

func configurationPackageAEAD(password string, salt []byte) (cipher.AEAD, []byte, error) {
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	key, err := scrypt.Key(passwordBytes, salt, configurationPackageScryptN, configurationPackageScryptR, configurationPackageScryptP, 32)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	return aead, key, nil
}
