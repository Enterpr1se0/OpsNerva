package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const MaxPrivateKeyBytes = 1 << 20

type nativeAuth struct {
	methods []ssh.AuthMethod
	closer  io.Closer
}

func prepareNativeAuthentication(ctx context.Context, host domain.Host) (nativeAuth, error) {
	switch host.AuthType {
	case "password":
		if host.Password == "" {
			return nativeAuth{}, fmt.Errorf("SSH password is unavailable")
		}
		if strings.ContainsAny(host.Password, "\x00\r\n") {
			return nativeAuth{}, fmt.Errorf("SSH password contains unsupported control characters")
		}
		password := host.Password
		answered := false
		keyboardInteractive := ssh.KeyboardInteractive(func(_ string, _ string, questions []string, echoes []bool) ([]string, error) {
			if answered || len(questions) != 1 || len(echoes) != 1 || echoes[0] {
				return nil, fmt.Errorf("keyboard-interactive authentication requested unsupported prompts")
			}
			question := strings.ToLower(strings.TrimSpace(questions[0]))
			if !strings.Contains(question, "password") && !strings.Contains(question, "passphrase") {
				return nil, fmt.Errorf("keyboard-interactive authentication requested a non-password response")
			}
			answered = true
			return []string{password}, nil
		})
		return nativeAuth{methods: []ssh.AuthMethod{ssh.Password(password), keyboardInteractive}}, nil
	case "key":
		signer, err := parsePrivateKey(host.PrivateKey)
		if err != nil {
			return nativeAuth{}, err
		}
		return nativeAuth{methods: []ssh.AuthMethod{ssh.PublicKeys(signer)}}, nil
	case "agent":
		connection, err := openSSHAgent(ctx)
		if err != nil {
			return nativeAuth{}, err
		}
		agentClient := agent.NewClient(connection)
		return nativeAuth{
			methods: []ssh.AuthMethod{ssh.PublicKeysCallback(agentClient.Signers)},
			closer:  connection,
		}, nil
	default:
		return nativeAuth{}, fmt.Errorf("unsupported SSH authentication type %q", host.AuthType)
	}
}

func ValidatePrivateKey(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("SSH private key is empty")
	}
	if len(data) > MaxPrivateKeyBytes {
		return fmt.Errorf("SSH private key exceeds 1 MiB")
	}
	_, err := parsePrivateKey(data)
	return err
}

func parsePrivateKey(data []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("encrypted SSH private keys cannot be uploaded; load the key into SSH Agent instead")
		}
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}
	return signer, nil
}
