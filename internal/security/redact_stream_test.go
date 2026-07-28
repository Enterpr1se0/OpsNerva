package security

import (
	"strings"
	"testing"
)

func TestStreamRedactorHoldsSplitSecretsUntilCompleteLine(t *testing.T) {
	stream := NewRedactor().NewStreamRedactor()
	if output := stream.Write([]byte("password=split-")); output != "" {
		t.Fatalf("incomplete line was emitted before redaction: %q", output)
	}
	output := stream.Write([]byte("secret\nready\n")) + stream.Flush()
	if strings.Contains(output, "split-secret") || output != "password=[REDACTED]\nready\n" {
		t.Fatalf("split secret was not safely redacted: %q", output)
	}
}

func TestStreamRedactorSuppressesPrivateKeyBlocks(t *testing.T) {
	stream := NewRedactor().NewStreamRedactor()
	output := stream.Write([]byte("before\n-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-"))
	output += stream.Write([]byte("material\n-----END OPENSSH PRIVATE KEY-----\nafter\n"))
	output += stream.Flush()
	if strings.Contains(output, "private-material") || output != "before\n[REDACTED]\nafter\n" {
		t.Fatalf("private key block was not safely redacted: %q", output)
	}
}

func TestStreamRedactorEmitsCarriageReturnProgress(t *testing.T) {
	stream := NewRedactor().NewStreamRedactor()
	output := stream.Write([]byte("progress 10%\rprogress 20%\r"))
	if output != "progress 10%\rprogress 20%\r" {
		t.Fatalf("carriage-return progress did not stream: %q", output)
	}
}
