package skills

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bodgit/sevenzip"
)

const (
	maxSkillPackageBytes   = 8 << 20
	maxSkillFileBytes      = 2 << 20
	maxSkillArchiveEntries = 4096
)

var (
	ErrNotFound = errors.New("skill not found")
	ErrDisabled = errors.New("skill is disabled")
	skillNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

const metadataFilename = "skill.json"

type metadata struct {
	Enabled bool `json:"enabled"`
}

// Registry stores administrator-managed skills as directories. Content is
// trusted as administrator-authored; validation here is limited to keeping the
// on-disk package structurally sound and inside the registry root.
type Registry struct {
	root       string
	initialize sync.Once
	initErr    error
	mu         sync.RWMutex
}

func NewRegistry(root string) *Registry {
	return &Registry{root: filepath.Clean(root)}
}

func (r *Registry) Root() string { return r.root }

func (r *Registry) List() ([]Skill, error) {
	if err := r.ensure(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, err
	}
	result := make([]Skill, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !validSkillName(entry.Name()) {
			continue
		}
		skill, err := r.readUnlocked(entry.Name(), false)
		if err != nil {
			continue
		}
		result = append(result, skill)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (r *Registry) Get(name string) (Skill, error) {
	if err := r.ensure(); err != nil {
		return Skill{}, err
	}
	if !validSkillName(name) {
		return Skill{}, fmt.Errorf("invalid skill name")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readUnlocked(name, true)
}

func (r *Registry) Save(name, skillContent string) (Skill, error) {
	if err := r.ensure(); err != nil {
		return Skill{}, err
	}
	name = strings.TrimSpace(name)
	if !validSkillName(name) {
		return Skill{}, fmt.Errorf("invalid skill name: use 1-64 letters, numbers, dots, underscores or hyphens")
	}
	data := []byte(skillContent)
	if len(bytes.TrimSpace(data)) == 0 {
		return Skill{}, fmt.Errorf("SKILL.md cannot be empty")
	}
	if len(data) > maxSkillFileBytes {
		return Skill{}, fmt.Errorf("SKILL.md exceeds 2 MiB")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	directory := filepath.Join(r.root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Skill{}, err
	}
	if err := atomicWrite(filepath.Join(directory, "SKILL.md"), data); err != nil {
		return Skill{}, err
	}
	return r.readUnlocked(name, true)
}

func (r *Registry) SetEnabled(name string, enabled bool) (Skill, error) {
	if err := r.ensure(); err != nil {
		return Skill{}, err
	}
	if !validSkillName(name) {
		return Skill{}, fmt.Errorf("invalid skill name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.readUnlocked(name, false); err != nil {
		return Skill{}, err
	}
	payload, err := json.MarshalIndent(metadata{Enabled: enabled}, "", "  ")
	if err != nil {
		return Skill{}, err
	}
	payload = append(payload, '\n')
	if err := atomicWrite(filepath.Join(r.root, name, metadataFilename), payload); err != nil {
		return Skill{}, err
	}
	return r.readUnlocked(name, true)
}

func (r *Registry) Import(name, filename string, source io.Reader) (Skill, error) {
	imported, err := r.ImportPackage(name, filename, source)
	if err != nil {
		return Skill{}, err
	}
	return imported[0], nil
}

func (r *Registry) ImportPackage(name, filename string, source io.Reader) ([]Skill, error) {
	if err := r.ensure(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	data, err := io.ReadAll(io.LimitReader(source, maxSkillPackageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSkillPackageBytes {
		return nil, fmt.Errorf("skill upload exceeds 8 MiB")
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md", ".markdown":
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
		}
		if !validSkillName(name) {
			return nil, fmt.Errorf("invalid skill name: use 1-64 letters, numbers, dots, underscores or hyphens")
		}
		skill, err := r.importMarkdown(name, data)
		if err != nil {
			return nil, err
		}
		return []Skill{skill}, nil
	case ".zip":
		return r.importZIP(name, filename, data)
	case ".7z":
		return r.import7Z(name, filename, data)
	default:
		return nil, fmt.Errorf("skill upload must be a Markdown, ZIP or 7z file")
	}
}

func (r *Registry) importMarkdown(name string, data []byte) (Skill, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Skill{}, fmt.Errorf("SKILL.md cannot be empty")
	}
	if len(data) > maxSkillFileBytes {
		return Skill{}, fmt.Errorf("SKILL.md exceeds 2 MiB")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	temporary, err := os.MkdirTemp(r.root, ".skill-upload-")
	if err != nil {
		return Skill{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.WriteFile(filepath.Join(temporary, "SKILL.md"), data, 0o600); err != nil {
		return Skill{}, err
	}
	if err := replaceDirectory(filepath.Join(r.root, name), temporary); err != nil {
		return Skill{}, err
	}
	committed = true
	return r.readUnlocked(name, true)
}

func (r *Registry) Delete(name string) error {
	if err := r.ensure(); err != nil {
		return err
	}
	if !validSkillName(name) {
		return fmt.Errorf("invalid skill name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target := filepath.Join(r.root, name)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid skill directory")
	}
	return os.RemoveAll(target)
}

func (r *Registry) ensure() error {
	r.initialize.Do(func() { r.initErr = r.initializeRoot() })
	return r.initErr
}

func (r *Registry) initializeRoot() error {
	if strings.TrimSpace(r.root) == "" || r.root == "." {
		return fmt.Errorf("skill registry root is not configured")
	}
	if info, err := os.Lstat(r.root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill registry root must be a real directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(r.root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".skills-initialize-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Rename(temporary, r.root); err != nil {
		if info, statErr := os.Lstat(r.root); statErr == nil && info.IsDir() {
			return nil
		}
		return err
	}
	committed = true
	return nil
}

func (r *Registry) readUnlocked(name string, includeContent bool) (Skill, error) {
	directory := filepath.Join(r.root, name)
	mainPath := filepath.Join(directory, "SKILL.md")
	info, err := os.Lstat(mainPath)
	if errors.Is(err, os.ErrNotExist) {
		return Skill{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return Skill{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Skill{}, fmt.Errorf("skill %q has an invalid SKILL.md", name)
	}
	data, err := os.ReadFile(mainPath)
	if err != nil {
		return Skill{}, err
	}
	if len(data) > maxSkillFileBytes {
		return Skill{}, fmt.Errorf("skill %q SKILL.md exceeds 2 MiB", name)
	}
	digest := sha256.Sum256(data)
	skill := Skill{Name: name, Summary: firstParagraph(string(data)), Enabled: true, ContentSHA256: fmt.Sprintf("%x", digest), UpdatedAt: info.ModTime().UTC().Format(time.RFC3339Nano)}
	metadataPath := filepath.Join(directory, metadataFilename)
	if metadataData, metadataErr := os.ReadFile(metadataPath); metadataErr == nil {
		var saved metadata
		if err := json.Unmarshal(metadataData, &saved); err != nil {
			return Skill{}, fmt.Errorf("skill %q has invalid %s: %w", name, metadataFilename, err)
		}
		skill.Enabled = saved.Enabled
	} else if !errors.Is(metadataErr, os.ErrNotExist) {
		return Skill{}, metadataErr
	}
	if includeContent {
		skill.Content = string(data)
	}
	err = filepath.WalkDir(directory, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill %q contains a symbolic link", name)
		}
		if entry.IsDir() {
			return nil
		}
		if currentPath == metadataPath {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		skill.FileCount++
		skill.SizeBytes += fileInfo.Size()
		if fileInfo.ModTime().UTC().Format(time.RFC3339Nano) > skill.UpdatedAt {
			skill.UpdatedAt = fileInfo.ModTime().UTC().Format(time.RFC3339Nano)
		}
		return nil
	})
	return skill, err
}

func (r *Registry) importZIP(name, filename string, data []byte) ([]Skill, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid skill ZIP: %w", err)
	}
	files := make([]skillArchiveFile, 0, len(reader.File))
	for _, file := range reader.File {
		files = append(files, skillArchiveFile{
			name: file.Name, mode: file.Mode(), uncompressedSize: file.UncompressedSize64, open: file.Open,
		})
	}
	return r.importArchive(name, filename, "ZIP", files)
}

func (r *Registry) import7Z(name, filename string, data []byte) ([]Skill, error) {
	reader, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid skill 7z: %w", err)
	}
	files := make([]skillArchiveFile, 0, len(reader.File))
	for _, file := range reader.File {
		files = append(files, skillArchiveFile{
			name: file.Name, mode: file.Mode(), uncompressedSize: file.UncompressedSize, open: file.Open,
		})
	}
	return r.importArchive(name, filename, "7z", files)
}

type skillArchiveFile struct {
	name             string
	mode             fs.FileMode
	uncompressedSize uint64
	open             func() (io.ReadCloser, error)
}

type skillArchiveRoot struct {
	name   string
	prefix string
}

func (r *Registry) importArchive(preferredName, filename, format string, files []skillArchiveFile) ([]Skill, error) {
	if len(files) == 0 || len(files) > maxSkillArchiveEntries {
		return nil, fmt.Errorf("skill %s must contain 1-%d entries", format, maxSkillArchiveEntries)
	}
	mainFiles := make([]string, 0)
	for _, file := range files {
		clean, err := cleanArchivePath(file.name, format)
		if err != nil {
			return nil, err
		}
		if path.Base(clean) == "SKILL.md" {
			mainFiles = append(mainFiles, clean)
		}
	}
	if len(mainFiles) == 0 {
		return nil, fmt.Errorf("skill %s must contain at least one SKILL.md", format)
	}
	sort.Strings(mainFiles)
	roots := make([]skillArchiveRoot, 0, len(mainFiles))
	names := make(map[string]string, len(mainFiles))
	for _, mainFile := range mainFiles {
		prefix := path.Dir(mainFile)
		name := ""
		if len(mainFiles) == 1 {
			name = strings.TrimSpace(preferredName)
		}
		if name == "" && prefix != "." {
			name = path.Base(prefix)
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
		}
		if !validSkillName(name) {
			return nil, fmt.Errorf("invalid Skill directory name %q in %s", name, format)
		}
		if previous, exists := names[name]; exists {
			return nil, fmt.Errorf("skill %s contains duplicate Skill name %q in %q and %q", format, name, previous, prefix)
		}
		names[name] = prefix
		roots = append(roots, skillArchiveRoot{name: name, prefix: prefix})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	staging, err := os.MkdirTemp(r.root, ".skill-upload-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	for _, root := range roots {
		if err := os.Mkdir(filepath.Join(staging, root.name), 0o700); err != nil {
			return nil, err
		}
	}
	var total int64
	for _, file := range files {
		clean, err := cleanArchivePath(file.name, format)
		if err != nil {
			return nil, err
		}
		owner, relative := archiveFileOwner(clean, roots)
		if owner == nil || relative == "" {
			continue
		}
		if relative == metadataFilename {
			return nil, fmt.Errorf("skill %s cannot contain reserved %s", format, metadataFilename)
		}
		if file.mode&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("skill %s cannot contain symbolic links", format)
		}
		target := filepath.Join(staging, owner.name, filepath.FromSlash(relative))
		if file.mode.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return nil, err
			}
			continue
		}
		if file.uncompressedSize > maxSkillFileBytes {
			return nil, fmt.Errorf("skill file %q exceeds 2 MiB", clean)
		}
		total += int64(file.uncompressedSize)
		if total > maxSkillPackageBytes {
			return nil, fmt.Errorf("expanded skill %s exceeds 8 MiB", format)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		input, err := file.open()
		if err != nil {
			return nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(input, maxSkillFileBytes+1))
		closeErr := input.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(payload) > maxSkillFileBytes {
			return nil, fmt.Errorf("skill file %q exceeds 2 MiB", clean)
		}
		if err := os.WriteFile(target, payload, 0o600); err != nil {
			return nil, err
		}
	}
	for _, root := range roots {
		mainData, err := os.ReadFile(filepath.Join(staging, root.name, "SKILL.md"))
		if err != nil || len(bytes.TrimSpace(mainData)) == 0 {
			return nil, fmt.Errorf("Skill %q has an empty or missing SKILL.md", root.name)
		}
	}
	replacements := make([]directoryReplacement, 0, len(roots))
	for _, root := range roots {
		replacements = append(replacements, directoryReplacement{
			target:      filepath.Join(r.root, root.name),
			replacement: filepath.Join(staging, root.name),
		})
	}
	if err := replaceDirectories(replacements); err != nil {
		return nil, err
	}
	result := make([]Skill, 0, len(roots))
	for _, root := range roots {
		skill, err := r.readUnlocked(root.name, true)
		if err != nil {
			return nil, err
		}
		result = append(result, skill)
	}
	return result, nil
}

func archiveFileOwner(clean string, roots []skillArchiveRoot) (*skillArchiveRoot, string) {
	ownerIndex := -1
	for index := range roots {
		prefix := roots[index].prefix
		if prefix == "." || clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			if ownerIndex < 0 || len(prefix) > len(roots[ownerIndex].prefix) {
				ownerIndex = index
			}
		}
	}
	if ownerIndex < 0 {
		return nil, ""
	}
	owner := &roots[ownerIndex]
	if owner.prefix == "." {
		return owner, clean
	}
	if clean == owner.prefix {
		return owner, ""
	}
	return owner, strings.TrimPrefix(clean, owner.prefix+"/")
}

func cleanArchivePath(value, format string) (string, error) {
	if value == "" || strings.Contains(value, `\`) || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("skill %s contains an invalid path", format)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("skill %s contains a path outside its root", format)
	}
	return clean, nil
}

type directoryReplacement struct {
	target      string
	replacement string
}

func replaceDirectories(replacements []directoryReplacement) error {
	if len(replacements) == 0 {
		return nil
	}
	for _, replacement := range replacements {
		info, err := os.Lstat(replacement.replacement)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill replacement is invalid")
		}
		if info, err := os.Lstat(replacement.target); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("existing skill target is invalid")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	rollbackRoot, err := os.MkdirTemp(filepath.Dir(replacements[0].target), ".skill-rollback-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(rollbackRoot) }()
	type replacementState struct {
		directoryReplacement
		backup    string
		hadTarget bool
		installed bool
	}
	states := make([]replacementState, len(replacements))
	rollback := func(last int) error {
		var rollbackErrors []error
		for index := last; index >= 0; index-- {
			state := states[index]
			if state.installed {
				if err := os.RemoveAll(state.target); err != nil {
					rollbackErrors = append(rollbackErrors, err)
					continue
				}
			}
			if state.hadTarget {
				if err := os.Rename(state.backup, state.target); err != nil {
					rollbackErrors = append(rollbackErrors, err)
				}
			}
		}
		return errors.Join(rollbackErrors...)
	}
	for index, replacement := range replacements {
		states[index].directoryReplacement = replacement
		states[index].backup = filepath.Join(rollbackRoot, fmt.Sprintf("%04d", index))
		if _, err := os.Lstat(replacement.target); err == nil {
			if err := os.Rename(replacement.target, states[index].backup); err != nil {
				return errors.Join(err, rollback(index-1))
			}
			states[index].hadTarget = true
		}
		if err := os.Rename(replacement.replacement, replacement.target); err != nil {
			return errors.Join(err, rollback(index))
		}
		states[index].installed = true
	}
	return nil
}

func replaceDirectory(target, replacement string) error {
	backup, err := os.MkdirTemp(filepath.Dir(target), ".skill-backup-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(backup) }()
	hadTarget := false
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("existing skill target is invalid")
		}
		if err := os.Rename(target, backup); err != nil {
			return err
		}
		hadTarget = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(replacement, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if hadTarget {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func atomicWrite(target string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".SKILL.md-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}

func validSkillName(name string) bool { return skillNameRE.MatchString(name) }
