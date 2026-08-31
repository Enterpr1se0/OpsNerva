package toolresult

import (
	"context"
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
