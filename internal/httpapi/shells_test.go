package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestSSHShellEventSequencePrefersReconnectHeader(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/ssh-shells/shell-1/events?after=0", nil)
	request.Header.Set("Last-Event-ID", "47")
	sequence, err := sshShellEventSequence(request)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 47 {
		t.Fatalf("reconnect sequence = %d, want 47", sequence)
	}

	initial := httptest.NewRequest("GET", "/api/v1/ssh-shells/shell-1/events?after=9", nil)
	sequence, err = sshShellEventSequence(initial)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 9 {
		t.Fatalf("initial sequence = %d, want 9", sequence)
	}
}
