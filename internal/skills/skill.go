package skills

import (
	"strings"

	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name           string `json:"name"`
	Summary        string `json:"summary"`
	Enabled        bool   `json:"enabled"`
	Content        string `json:"content,omitempty"`
	BaseDirectory  string `json:"-"`
	RuntimeContent string `json:"-"`
	ContentSHA256  string `json:"content_sha256,omitempty"`
	FileCount      int    `json:"file_count,omitempty"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type skillFrontMatter struct {
	Description string `yaml:"description"`
}

func parseSkillDocument(text string) (summary, content string) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	trimmed := strings.TrimSpace(normalized)
	if !strings.HasPrefix(trimmed, "---\n") {
		return firstParagraph(trimmed), trimmed
	}
	rest := strings.TrimPrefix(trimmed, "---\n")
	closing := strings.Index(rest, "\n---")
	if closing < 0 {
		return firstParagraph(trimmed), trimmed
	}
	after := rest[closing+len("\n---"):]
	if after != "" && !strings.HasPrefix(after, "\n") {
		return firstParagraph(trimmed), trimmed
	}
	var frontMatter skillFrontMatter
	if err := yaml.Unmarshal([]byte(rest[:closing]), &frontMatter); err != nil {
		return firstParagraph(trimmed), trimmed
	}
	content = strings.TrimSpace(after)
	summary = strings.Join(strings.Fields(frontMatter.Description), " ")
	if summary == "" {
		summary = firstParagraph(content)
	}
	return summary, content
}

func firstParagraph(text string) string {
	parts := strings.Split(strings.TrimSpace(text), "\n\n")
	if len(parts) < 2 {
		return strings.TrimSpace(text)
	}
	return strings.Join(strings.Fields(parts[1]), " ")
}
