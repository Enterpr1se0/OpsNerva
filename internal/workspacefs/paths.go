package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FS provides local file operations rooted at one managed Workspace.
// It does not own registration, access policy, approval, audit, or shell state.
// Resolve retains the existing symlink checks; FS is not an OS-level sandbox.
type FS struct{ root string }

func New(root string) *FS { return &FS{root: root} }

// InvalidPathError marks malformed relative input, not filesystem failures.
type InvalidPathError struct{ Message string }

func (err *InvalidPathError) Error() string { return err.Message }

func (fs *FS) Resolve(relative string, allowMissing bool) (string, error) {
	relative = NormalizeRelativePath(relative)
	localRelative := filepath.FromSlash(relative)
	if path.IsAbs(relative) || filepath.IsAbs(localRelative) {
		return "", &InvalidPathError{Message: `workspace path must be relative; omit path or use "." for the Workspace root (examples: "src", "src/main.go"); absolute paths such as "/workspace" are invalid`}
	}
	if path.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\\\x00\r\n") {
		return "", &InvalidPathError{Message: `workspace path must be clean and relative (examples: ".", "src", "src/main.go")`}
	}
	for _, component := range strings.Split(relative, "/") {
		if IsSensitiveComponent(component) {
			return "", fmt.Errorf("workspace path is sensitive and denied")
		}
	}
	root, err := filepath.EvalSymlinks(fs.root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	target := filepath.Join(root, localRelative)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if !allowMissing || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(target))
		if parentErr != nil {
			return "", parentErr
		}
		resolved = filepath.Join(parent, filepath.Base(target))
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace path escapes its configured root")
	}
	if info, lstatErr := os.Lstat(target); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace symbolic links are denied")
	}
	return resolved, nil
}

func NormalizeRelativePath(relative string) string {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "."
	}
	return relative
}

func IsSensitiveComponent(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".env") || strings.HasPrefix(lower, ".opsnerva-") || lower == ".ssh" || lower == "data" || lower == "master.key" || strings.Contains(lower, "credential")
}
