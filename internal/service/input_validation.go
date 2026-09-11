package service

import (
	"errors"
	"strings"
)

// InputValidationError identifies a rejected tool contract without relying on
// message text. Agent and MCP adapters expose it as a non-retryable failure.
type InputValidationError struct {
	err error
}

func (err *InputValidationError) Error() string { return err.err.Error() }

func (err *InputValidationError) Unwrap() error { return err.err }

func asInputValidationError(err error) error {
	if err == nil {
		return nil
	}
	var validation *InputValidationError
	var selection *ExecutionToolSelectionError
	if errors.As(err, &validation) || errors.As(err, &selection) {
		return err
	}
	return &InputValidationError{err: err}
}

func containsCredentialControl(value string) bool {
	return strings.ContainsAny(value, "\x00\r\n")
}
