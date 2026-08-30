package agenttool

import (
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// InputError carries a rejected tool contract and optional correction data.
type InputError struct {
	message    string
	validation *domain.ToolValidationDetails
}

func (err *InputError) Error() string { return err.message }

func (err *InputError) Validation() *domain.ToolValidationDetails {
	if err == nil || err.validation == nil {
		return nil
	}
	copy := *err.validation
	copy.AllowedFields = append([]string(nil), err.validation.AllowedFields...)
	copy.GotFields = append([]string(nil), err.validation.GotFields...)
	copy.UnexpectedFields = append([]string(nil), err.validation.UnexpectedFields...)
	return &copy
}

func InvalidInput(format string, arguments ...any) error {
	return &InputError{message: fmt.Sprintf(format, arguments...)}
}

func StructuredInputError(message string, validation domain.ToolValidationDetails) error {
	return &InputError{message: message, validation: &validation}
}

func ValidateActionFields(action string, provided, allowed []string, example map[string]any) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	unexpected := make([]string, 0)
	for _, field := range provided {
		if _, ok := allowedSet[field]; !ok {
			unexpected = append(unexpected, field)
		}
	}
	if len(unexpected) == 0 {
		return nil
	}
	return StructuredInputError(
		fmt.Sprintf("action=%s received unsupported fields: %s", action, strings.Join(unexpected, ", ")),
		domain.ToolValidationDetails{
			Action: action, AllowedFields: allowed, GotFields: provided,
			UnexpectedFields: unexpected, Example: example,
		},
	)
}
