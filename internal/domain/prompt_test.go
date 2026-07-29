package domain

import (
	"strings"
	"testing"
)

func TestDefaultSystemPromptUsesWebBeforeUnspecifiedLocalProjectDiscovery(t *testing.T) {
	for _, instruction := range []string{
		"Workspace binding does not prove a project is local",
		"Without an explicit local statement or Workspace path",
		"use web_search first",
		"then web_extract official documentation",
	} {
		if !strings.Contains(DefaultSystemPrompt, instruction) {
			t.Fatalf("default system prompt is missing project discovery instruction %q", instruction)
		}
	}
}

func TestDefaultSystemPromptKeepsHardOperationalRules(t *testing.T) {
	for _, instruction := range []string{
		"Call only listed tools",
		"call ops_plan_create first with 2-8 verifiable steps",
		"Execute only the current in_progress step",
		"Never request credentials",
		"never run sudo or include passwords in tool input",
		"Policy and the configured approval mode are authoritative",
		"never retry that operation in the same run",
		"Never bypass policy",
		"workspace_* may access only the conversation-bound Workspace",
		"mcp__ tools are outside SSH policy",
	} {
		if !strings.Contains(DefaultSystemPrompt, instruction) {
			t.Fatalf("default system prompt is missing hard operational rule %q", instruction)
		}
	}
}
