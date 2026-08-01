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
		"Execute only the current step",
		"use ops_plan_revise only when unfinished scope or order changes",
		"Never request credentials",
		"never run sudo or include passwords in tool input",
		"Server validation and the configured approval mode are authoritative",
		"File reads are paged by default",
		"follow file.next_offset while file.has_more",
		"never retry that operation in the same run",
		"Never bypass validation or approval",
		"workspace_* may access only the conversation-bound Workspace",
		"mcp__ tools are outside OpsNerva approval controls",
		"omit destination sha256 to create",
	} {
		if !strings.Contains(DefaultSystemPrompt, instruction) {
			t.Fatalf("default system prompt is missing hard operational rule %q", instruction)
		}
	}
}
