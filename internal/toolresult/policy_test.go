package toolresult

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestPolicyPreservesValidationDetails(t *testing.T) {
	err := agenttool.StructuredInputError("unsupported field", domain.ToolValidationDetails{
		Action: "list", UnexpectedFields: []string{"host_id"},
	})
	result, normalizeErr := CompactExec(domain.ExecResult{}, err)
	if normalizeErr != nil {
		t.Fatal(normalizeErr)
	}
	if result.Code != "validation_failed" || result.Validation == nil || result.Validation.Action != "list" {
		t.Fatalf("normalized validation result = %#v", result)
	}
	value, normalizeErr := (Policy{}).Value(context.Background(), "ssh_tunnel", nil, err)
	if normalizeErr != nil {
		t.Fatal(normalizeErr)
	}
	failure, ok := value.(domain.ToolFailure)
	if !ok || failure.Validation == nil || len(failure.Validation.UnexpectedFields) != 1 {
		t.Fatalf("normalized validation failure = %#v", value)
	}
}

func TestPolicyExplainsExactFileEditWhitespace(t *testing.T) {
	result, err := CompactExec(domain.ExecResult{Status: "failed", ExitCode: 75}, errors.New("file edit conflict: old_text matched 0 blocks"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "conflict" || !result.Retryable || !strings.Contains(result.NextAction, "preserving all leading whitespace") {
		t.Fatalf("normalized file edit conflict = %#v", result)
	}
}
