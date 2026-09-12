package workspaces

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func validateIdentity(id, access string) error {
	if !workspaceIDPattern.MatchString(id) || id == "." || id == ".." || strings.HasSuffix(id, ".") || isReservedWindowsID(id) {
		return fmt.Errorf("workspace id must use 1-64 letters, numbers, dots, underscores, or hyphens")
	}
	if access != "read_only" && access != "read_write" {
		return fmt.Errorf("workspace access must be read_only or read_write")
	}
	return nil
}

func isReservedWindowsID(id string) bool {
	base := strings.ToUpper(strings.SplitN(id, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("workspace directories cannot contain symbolic links")
	}
	return nil
}

func pathContains(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		relative = strings.ToLower(relative)
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
