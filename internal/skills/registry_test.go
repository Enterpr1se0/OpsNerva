package skills

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryInitializesEmptyAndKeepsPermanentDeletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	registry := NewRegistry(root)
	items, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("new registry contains default skills: %#v", items)
	}
	if _, err := registry.Save("temporary", "# Temporary\n\nDelete this workflow."); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete("temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get("temporary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted skill error=%v", err)
	}

	restarted := NewRegistry(root)
	items, err = restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("permanently deleted skill reappeared: %#v", items)
	}
}

func TestRegistrySavesAndImportsManagedSkills(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	saved, err := registry.Save("redis-recovery", "# Redis Recovery\n\nInspect persistence and bounded logs first.")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "redis-recovery" || saved.Content == "" || saved.ContentSHA256 == "" || saved.FileCount != 1 {
		t.Fatalf("unexpected saved skill: %#v", saved)
	}

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	mainFile, _ := writer.Create("kubernetes-debug/SKILL.md")
	_, _ = mainFile.Write([]byte("# Kubernetes Debug\n\nInspect events before changing workloads."))
	reference, _ := writer.Create("kubernetes-debug/references/events.md")
	_, _ = reference.Write([]byte("Use bounded event windows."))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	imported, err := registry.Import("kubernetes-debug", "kubernetes-debug.zip", bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported.FileCount != 2 || imported.Content == "" {
		t.Fatalf("unexpected imported skill: %#v", imported)
	}
	if _, err := os.Stat(filepath.Join(registry.Root(), "kubernetes-debug", "references", "events.md")); err != nil {
		t.Fatal(err)
	}
	replaced, err := registry.Import("kubernetes-debug", "replacement.md", bytes.NewBufferString("# Replacement\n\nUse the replacement workflow."))
	if err != nil {
		t.Fatal(err)
	}
	if replaced.FileCount != 1 {
		t.Fatalf("Markdown upload did not replace the prior package: %#v", replaced)
	}
	if _, err := os.Stat(filepath.Join(registry.Root(), "kubernetes-debug", "references", "events.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old package reference survived replacement: %v", err)
	}
}

func TestRegistryRejectsZIPPathTraversal(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	mainFile, _ := writer.Create("SKILL.md")
	_, _ = mainFile.Write([]byte("# Valid main"))
	escape, _ := writer.Create("../escaped.txt")
	_, _ = escape.Write([]byte("escape"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Import("invalid", "invalid.zip", bytes.NewReader(archive.Bytes())); err == nil {
		t.Fatal("path-traversing ZIP was accepted")
	}
}

func TestRegistryImportsSkillZIPWithManyReferences(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	mainFile, err := writer.Create("cloudflare/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mainFile.Write([]byte("# Cloudflare\n\nUse the bundled references.")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 320; i++ {
		reference, err := writer.Create(fmt.Sprintf("cloudflare/references/product-%03d.md", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reference.Write([]byte("reference")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	imported, err := registry.Import("cloudflare", "cloudflare.zip", bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported.FileCount != 321 {
		t.Fatalf("imported file count=%d, want 321", imported.FileCount)
	}
}

func TestRegistryImportsMultipleSkillsWithTheirResources(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entries := map[string]string{
		"cloudflare-skills/README.md":                               "repository metadata",
		"cloudflare-skills/skills/cloudflare/SKILL.md":              "# Cloudflare\n\nCloudflare workflow.",
		"cloudflare-skills/skills/cloudflare/references/workers.md": "Workers reference.",
		"cloudflare-skills/skills/wrangler/SKILL.md":                "# Wrangler\n\nWrangler workflow.",
		"cloudflare-skills/skills/wrangler/scripts/check.sh":        "#!/bin/sh\necho ready\n",
	}
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	imported, err := registry.ImportPackage("", "cloudflare-skills.zip", bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 || imported[0].Name != "cloudflare" || imported[1].Name != "wrangler" {
		t.Fatalf("unexpected imported Skills: %#v", imported)
	}
	for _, target := range []string{
		filepath.Join(registry.Root(), "cloudflare", "references", "workers.md"),
		filepath.Join(registry.Root(), "wrangler", "scripts", "check.sh"),
	} {
		if _, err := os.Stat(target); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(registry.Root(), "cloudflare", "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file outside a Skill directory was imported: %v", err)
	}
}

func TestRegistryRejectsWholePackageBeforeReplacingAnySkill(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	if _, err := registry.Save("cloudflare", "# Existing\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry.Root(), "wrangler"), []byte("invalid target"), 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range map[string]string{
		"skills/cloudflare/SKILL.md": "# Replacement\n",
		"skills/wrangler/SKILL.md":   "# Wrangler\n",
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.ImportPackage("", "skills.zip", bytes.NewReader(archive.Bytes())); err == nil {
		t.Fatal("package with an invalid target was imported")
	}
	existing, err := registry.Get("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if existing.Content != "# Existing\n" {
		t.Fatalf("existing Skill was partially replaced: %q", existing.Content)
	}
}

func TestRegistryImports7ZSkillPackage(t *testing.T) {
	const encoded = "N3q8ryccAAQy4d37ywAAAAAAAAAiAAAAAAAAAEQ8yhkBACYjIENsb3VkZmxhcmUKClVzZSBidW5kbGVkIHJlZmVyZW5jZXMuCgoAAQATUmVmZXJlbmNlIGNvbnRlbnQuCgoAAACBMweuMZrlb/NKeGsqvHGNB3BrXj5XDKF7PgMlD05meTvCS0IbduvWXcVlA3RKG4U9VepJBH+CPu9831Y+DPs/ftwCouZrYSpaA0o1XNWQxJq9P5cfvkBo3qhgPGmGsLL56kc3suF3RvH6a6tqqniEC31PheGznXG042N/dZipepoGL5gAABcGQwEJgIgABwsBAAEjAwEBBV0AEAAADIC+CgF0vVf0AAA="
	archive, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	imported, err := registry.Import("cloudflare", "cloudflare.7z", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if imported.FileCount != 2 || !strings.Contains(imported.Content, "# Cloudflare") {
		t.Fatalf("unexpected imported 7z skill: %#v", imported)
	}
	if _, err := os.Stat(filepath.Join(registry.Root(), "cloudflare", "references", "example.md")); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRejectsZIPWithTooManyEntries(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "skills"))
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for i := 0; i <= maxSkillArchiveEntries; i++ {
		_, err := writer.Create(fmt.Sprintf("skill/file-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := registry.Import("too-many", "too-many.zip", bytes.NewReader(archive.Bytes()))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("1-%d entries", maxSkillArchiveEntries)) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegistryEnabledStatePersists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	registry := NewRegistry(root)
	if _, err := registry.Save("custom-diagnosis", "# Custom Diagnosis\n\nInspect the reported failure."); err != nil {
		t.Fatal(err)
	}
	disabled, err := registry.SetEnabled("custom-diagnosis", false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled {
		t.Fatalf("disabled skill is enabled: %#v", disabled)
	}

	restarted := NewRegistry(root)
	loaded, err := restarted.Get("custom-diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Enabled {
		t.Fatalf("disabled state did not persist: %#v", loaded)
	}
	enabled, err := restarted.SetEnabled("custom-diagnosis", true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.FileCount != 1 {
		t.Fatalf("unexpected enabled skill metadata: %#v", enabled)
	}
}
