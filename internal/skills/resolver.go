package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ResolveErrorCode classifies failures while resolving a skill URI.
type ResolveErrorCode string

const (
	ResolveInvalidURI   ResolveErrorCode = "invalid_uri"
	ResolveUnknownSkill ResolveErrorCode = "unknown_skill"
	ResolveInvalidPath  ResolveErrorCode = "invalid_path"
	ResolvePathEscape   ResolveErrorCode = "path_escape"
)

// ResolveError is a typed skill URI resolution failure. Path is always logical
// to the skill and never exposes the skill's disk location.
type ResolveError struct {
	Code  ResolveErrorCode
	URI   string
	Skill string
	Path  string
	Err   error
}

func (e *ResolveError) Error() string {
	if e.Skill != "" {
		return fmt.Sprintf("resolve %s for skill %q: %s", e.URI, e.Skill, e.Code)
	}
	return fmt.Sprintf("resolve %s: %s", e.URI, e.Code)
}

func (e *ResolveError) Unwrap() error { return e.Err }

// Is matches a resolution failure by code.
func (e *ResolveError) Is(target error) bool {
	other, ok := target.(*ResolveError)
	return ok && e.Code == other.Code
}

// Resolver is an immutable index of canonical skill metadata. It locates
// skill:// URIs; internal/refs reads them.
type Resolver struct {
	items map[string]Skill
}

// NewResolver validates and indexes a canonical skill slice.
func NewResolver(items []Skill) (*Resolver, error) {
	index := make(map[string]Skill, len(items))
	for _, skill := range items {
		if skill.Name == "" || len(ValidateName(skill.Name)) != 0 {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("invalid skill name")}
		}
		if !filepath.IsAbs(skill.FilePath) || !filepath.IsAbs(skill.BaseDir) {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("skill paths must be absolute")}
		}
		rel, err := filepath.Rel(skill.BaseDir, skill.FilePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("skill file is outside its base directory")}
		}
		if _, exists := index[skill.Name]; exists {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("duplicate skill name")}
		}
		index[skill.Name] = skill
	}
	return &Resolver{items: index}, nil
}

// Location is a validated skill:// target: the skill's base directory and
// the cleaned path inside it. Locate does no I/O; callers open Rel through
// an [os.Root] on BaseDir so symlinks cannot leave the skill.
type Location struct {
	Skill   string
	BaseDir string
	// Rel is the OS-cleaned path relative to BaseDir; "." is BaseDir itself.
	Rel string
	// Logical is Rel in slash form, safe to show without the disk location.
	Logical string
}

// Locate parses and validates a skill:// URI without touching the disk.
// skill://name and skill://name/ name the skill's own file.
func (r *Resolver) Locate(rawURI string) (Location, error) {
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme != "skill" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURI, "#") || u.Host == "" {
		return Location{}, &ResolveError{Code: ResolveInvalidURI, URI: rawURI, Err: err}
	}
	skill, ok := r.items[u.Host]
	if !ok {
		return Location{}, &ResolveError{Code: ResolveUnknownSkill, URI: rawURI, Skill: u.Host, Err: fs.ErrNotExist}
	}
	var target string
	if u.EscapedPath() == "" || u.EscapedPath() == "/" {
		target, err = filepath.Rel(skill.BaseDir, skill.FilePath)
		if err != nil {
			return Location{}, &ResolveError{Code: ResolveInvalidPath, URI: rawURI, Skill: skill.Name, Err: err}
		}
	} else {
		escaped := strings.TrimPrefix(u.EscapedPath(), "/")
		target, err = url.PathUnescape(escaped)
		if err != nil {
			return Location{}, &ResolveError{Code: ResolveInvalidURI, URI: rawURI, Skill: skill.Name, Err: err}
		}
	}
	logical := filepath.ToSlash(target)
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.ContainsRune(target, 0) || strings.Contains(target, `\`) {
		return Location{}, &ResolveError{Code: ResolveInvalidPath, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("invalid asset path")}
	}
	for _, part := range strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part == ".." {
			return Location{}, &ResolveError{Code: ResolvePathEscape, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("path traversal")}
		}
	}
	target = filepath.Clean(target)
	return Location{Skill: skill.Name, BaseDir: skill.BaseDir, Rel: target, Logical: filepath.ToSlash(target)}, nil
}

// TargetEscapesBase reports whether rel, resolved through symlinks, leaves
// base. It classifies [os.Root] refusals for error messages; the root
// remains the enforcement. A base behind a symlinked prefix (macOS /var)
// is compared in both forms.
func TargetEscapesBase(base, rel string) bool {
	outside := func(target string) bool {
		if !outsideBase(base, target) {
			return false
		}
		evaluated, err := filepath.EvalSymlinks(base)
		return err != nil || outsideBase(evaluated, target)
	}
	path := filepath.Join(base, rel)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return outside(resolved)
	}
	info, lstatErr := os.Lstat(path)
	if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	link, linkErr := os.Readlink(path)
	if linkErr != nil {
		return false
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	return outside(filepath.Clean(link))
}

// outsideBase uses path-aware containment so sibling directories sharing a prefix don't count as children.
func outsideBase(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative))
}

// SkillNames returns sorted indexed names for diagnostics without exposing paths.
func (r *Resolver) SkillNames() []string {
	names := make([]string, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
