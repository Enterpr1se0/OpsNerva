package agent

import (
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestDecodeExplanationJSONAndRejectUnexpectedFields(t *testing.T) {
	var explanation domain.CommandExplanation
	response := "```json\n{\"summary\":\"重启服务\",\"mechanism\":\"systemd restarts the unit\",\"risks\":[\"requests may fail\"]}\n```"
	if err := decodeJSONObject(response, &explanation); err != nil {
		t.Fatal(err)
	}
	if explanation.Summary != "重启服务" || len(explanation.Risks) != 1 {
		t.Fatalf("unexpected structured explanation: %#v", explanation)
	}
	if err := decodeJSONObject(`{"summary":"test","unexpected":true}`, &explanation); err == nil {
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

func TestExplanationBoundsListsAndText(t *testing.T) {
	values := make([]string, 10)
	for index := range values {
		values[index] = " item "
	}
	if bounded := boundedStrings(values); len(bounded) != 8 || bounded[0] != "item" {
		t.Fatalf("explanation list was not bounded: %#v", bounded)
	}
}
