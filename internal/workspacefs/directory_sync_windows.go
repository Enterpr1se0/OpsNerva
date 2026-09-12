//go:build windows

package workspacefs

// Windows does not expose portable directory fsync semantics through os.File.
// File contents are synced before every rename or hard-link that reaches here.
func syncDirectory(string) error {
	return nil
}
