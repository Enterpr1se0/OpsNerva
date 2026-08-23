package security

import (
	"bytes"
	"errors"
	"testing"
)

func TestConfigurationPackageEncryptionRoundTrip(t *testing.T) {
	plain := []byte(`{"api_key":"migration-secret"}`)
	payload, err := EncryptConfigurationPackage(plain, "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, plain) || bytes.Contains(payload, []byte("migration-secret")) {
		t.Fatal("encrypted configuration package contains plaintext")
	}
	second, err := EncryptConfigurationPackage(plain, "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(payload, second) {
		t.Fatal("configuration package encryption reused its salt or nonce")
	}
	decoded, err := DecryptConfigurationPackage(payload, "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatalf("decoded package = %q", decoded)
	}
}

func TestConfigurationPackageEncryptionRejectsWrongPasswordAndTampering(t *testing.T) {
	payload, err := EncryptConfigurationPackage([]byte("configuration"), "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptConfigurationPackage(payload, "different-password"); !errors.Is(err, ErrConfigurationPackageAuthentication) {
		t.Fatalf("wrong password error = %v", err)
	}
	payload[len(payload)-1] ^= 1
	if _, err := DecryptConfigurationPackage(payload, "migration-password"); !errors.Is(err, ErrConfigurationPackageAuthentication) {
		t.Fatalf("tampered package error = %v", err)
	}
}

func TestConfigurationPackageEncryptionRejectsInvalidInput(t *testing.T) {
	if _, err := EncryptConfigurationPackage([]byte("configuration"), "short"); err == nil {
		t.Fatal("short password was accepted")
	}
	if _, err := DecryptConfigurationPackage([]byte("plain JSON"), "migration-password"); err == nil {
		t.Fatal("plain package was accepted")
	}
}
