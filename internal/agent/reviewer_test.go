package agent

import (
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestApprovalAgentsUseIndependentInstructions(t *testing.T) {
	if strings.Contains(explainerInstruction, "user_request") || strings.Contains(explainerInstruction, `"manual"`) {
		t.Fatal("the existing explanation Agent instruction was changed into an Auto decision instruction")
	}
	for _, required := range []string{"user_request", `"allow"`, `"reject"`, `"manual"`} {
		if !strings.Contains(automaticApprovalInstruction, required) {
			t.Fatalf("Auto approval Agent instruction is missing %q", required)
		}
	}
}

func TestDecodeApprovalReviewJSONAndRejectUnexpectedFields(t *testing.T) {
	var review struct {
		Decision  string   `json:"decision"`
		Reason    string   `json:"reason"`
		Summary   string   `json:"summary"`
		Mechanism string   `json:"mechanism"`
		Risks     []string `json:"risks"`
	}
	response := "```json\n{\"decision\":\"allow\",\"reason\":\"范围明确\",\"summary\":\"重启服务\",\"mechanism\":\"systemd restarts the unit\",\"risks\":[\"requests may fail\"]}\n```"
	if err := decodeJSONObject(response, &review); err != nil {
		t.Fatal(err)
	}
	if review.Decision != domain.ApprovalAgentAllow || review.Summary != "重启服务" || len(review.Risks) != 1 {
		t.Fatalf("unexpected structured review: %#v", review)
	}
	if err := decodeJSONObject(`{"decision":"allow","unexpected":true}`, &review); err == nil {
		t.Fatal("expected an unknown structured field to be rejected")
	}
}

func TestReviewInputMasksEnvironmentValues(t *testing.T) {
	input := domain.CommandReviewInput{Request: domain.ExecRequest{Env: map[string]string{"TOKEN": "secret", "MODE": "prod"}}}
	masked := maskExplanationInput(input)
	if masked.Request.Env["TOKEN"] != "[configured]" || masked.Request.Env["MODE"] != "[configured]" {
		t.Fatalf("environment values were exposed to the explanation Agent: %#v", masked.Request.Env)
	}
	if input.Request.Env["TOKEN"] != "secret" {
		t.Fatal("masking mutated the execution request")
	}
}

func TestAutomaticApprovalInputMasksEnvironmentValues(t *testing.T) {
	input := domain.AutomaticApprovalInput{Request: domain.ExecRequest{Env: map[string]string{"TOKEN": "secret"}}, UserRequest: "deploy demo"}
	masked := maskAutomaticApprovalInput(input)
	if masked.Request.Env["TOKEN"] != "[configured]" || masked.UserRequest != input.UserRequest {
		t.Fatalf("automatic approval input was not masked correctly: %#v", masked)
	}
	if input.Request.Env["TOKEN"] != "secret" {
		t.Fatal("automatic approval masking mutated the execution request")
	}
}

func TestExplanationBoundsListsAndText(t *testing.T) {
	values := make([]string, 10)
	for index := range values {
		values[index] = " item "
	}
	if bounded := boundedStrings(values); len(bounded) != 8 || bounded[0] != "item" {
		t.Fatalf("explanation list was not bounded: %#v", bounded)
	}
}
