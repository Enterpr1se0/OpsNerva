package skills

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSkillDocumentUsesFrontmatterDescriptionAndBody(t *testing.T) {
	summary, content := parseSkillDocument("---\nname: demo\ndescription:  Diagnose   services safely.\ncontext: fork\n---\n\n# Workflow\n\nInspect first.")
	if summary != "Diagnose services safely." {
		t.Fatalf("summary = %q", summary)
	}
	if content != "# Workflow\n\nInspect first." {
		t.Fatalf("runtime content = %q", content)
	}
}

func TestRuntimeSkillPathsRemainOutsideAdminJSON(t *testing.T) {
	payload, err := json.Marshal(Skill{Name: "demo", BaseDirectory: "/private/skills/demo", RuntimeContent: "runtime only"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "private") || strings.Contains(string(payload), "runtime only") {
		t.Fatalf("runtime-only fields leaked into admin JSON: %s", payload)
	}
}
