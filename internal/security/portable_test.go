package security

import (
	"bytes"
	"errors"
	"testing"
)

func TestPortableEncryptionRoundTripAndRejectsWrongPassword(t *testing.T) {
	plain := []byte(`{"api_key":"portable-secret"}`)
	sealed, err := SealPortable("correct horse battery staple", plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("portable-secret")) {
		t.Fatal("encrypted migration package exposed plaintext")
	}
	opened, encrypted, err := OpenPortable("correct horse battery staple", sealed)
	if err != nil || !encrypted || !bytes.Equal(opened, plain) {
		t.Fatalf("open portable package = encrypted %v, body %q, error %v", encrypted, opened, err)
	}
	if _, encrypted, err := OpenPortable("wrong password", sealed); !encrypted || !errors.Is(err, ErrPortablePassword) {
		t.Fatalf("wrong password = encrypted %v, error %v", encrypted, err)
	}
	if body, encrypted, err := OpenPortable("", plain); err != nil || encrypted || body != nil {
		t.Fatalf("plain package detection = encrypted %v, body %q, error %v", encrypted, body, err)
	}
}
