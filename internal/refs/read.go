package refs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"go.harness.dev/harness/internal/skills"
)

// MaxEntries bounds one directory listing.
const MaxEntries = 500

// Entry is one directory entry.
type Entry struct {
	Name string
	Dir  bool
	Size int64
}

// Resource is what a URL read yields: file bytes, a directory listing, or
// text rendered from the session journals.
type Resource struct {
	URI string
	// Data holds the file bytes, or the rendered text when Rendered is set.
	Data     []byte
	Rendered bool
	// Dir marks a directory; Entries holds at most MaxEntries of Total
	// entries, sorted by name.
	Dir     bool
	Entries []Entry
	Total   int
}

// Read resolves raw and loads it. Files must be regular and at most
// MaxFileBytes; named pipes and devices are refused without blocking.
func (r *Resolver) Read(ctx context.Context, raw string) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	ref, err := Parse(raw)
	if err != nil {
		return Resource{}, err
	}
	switch {
	case ref.Scheme == SchemeJob && ref.Name == "":
		return r.renderJobs(ctx, ref)
	case ref.Scheme == SchemeJob && ref.Path == "" && !ref.IsDirForm():
		return r.renderJob(ctx, ref)
	case ref.Scheme == SchemeHistory && ref.Name == "":
		return r.renderChildren(ctx, ref)
	case ref.Scheme == SchemeHistory:
		return r.renderHistory(ctx, ref)
	}
	location, err := r.Locate(ref)
	if err != nil {
		return Resource{}, err
	}
	if err := r.prepareRoot(ref, location); err != nil {
		return Resource{}, err
	}
	return readLocation(ctx, location)
}

// prepareRoot creates the local:// directory on first use and refuses a
// scheme root under the session data directory that is a symlink.
func (r *Resolver) prepareRoot(ref Ref, location Location) error {
	switch ref.Scheme {
	case SchemeLocal:
		if err := os.MkdirAll(location.Root, 0o700); err != nil {
			return newError(CodeReadFailed, location.URI, "cannot create the local:// directory", err)
		}
	case SchemeJob:
		if _, err := os.Lstat(location.Root); errors.Is(err, fs.ErrNotExist) {
			return newError(CodeNotFound, location.URI, fmt.Sprintf("job %s has no artifacts in this session tree", ref.Name), fs.ErrNotExist)
		}
	default:
		return nil
	}
	for dir := location.Root; dir != r.RootDataDir() && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return newError(CodeReadFailed, location.URI, "cannot inspect the scheme directory", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return newError(CodeEscape, location.URI, "the scheme directory is not a plain directory", nil)
		}
	}
	return nil
}

// readLocation opens location through an os.Root on its scheme root.
func readLocation(ctx context.Context, location Location) (Resource, error) {
	root, err := os.OpenRoot(location.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Resource{}, newError(CodeNotFound, location.URI, "not found", err)
		}
		return Resource{}, newError(CodeReadFailed, location.URI, "cannot open the scheme directory", err)
	}
	defer root.Close()
	info, err := root.Stat(location.Name)
	if err != nil {
		return Resource{}, rootError(location, err)
	}
	if info.IsDir() {
		return listDir(ctx, root, location)
	}
	if !info.Mode().IsRegular() {
		return Resource{}, newError(CodeTarget, location.URI, "not a regular file or directory", nil)
	}
	if info.Size() > MaxFileBytes {
		return Resource{}, tooLarge(location, info.Size())
	}
	// O_NONBLOCK keeps a file swapped for a FIFO after the stat from
	// blocking the open; the fstat below then refuses it.
	file, err := root.OpenFile(location.Name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Resource{}, rootError(location, err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return Resource{}, newError(CodeReadFailed, location.URI, "cannot stat the file", err)
	}
	if !info.Mode().IsRegular() {
		return Resource{}, newError(CodeTarget, location.URI, "not a regular file or directory", nil)
	}
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return Resource{}, newError(CodeReadFailed, location.URI, "read failed", err)
	}
	if len(data) > MaxFileBytes {
		return Resource{}, tooLarge(location, int64(len(data)))
	}
	return Resource{URI: location.URI, Data: data}, nil
}

func tooLarge(location Location, size int64) error {
	return newError(CodeTooLarge, location.URI, fmt.Sprintf("file is %d bytes, over the %d MiB read limit; use bash on it (URLs in bash commands expand to paths)", size, MaxFileBytes>>20), nil)
}

// rootError classifies an os.Root refusal. The root already refused the
// access; this only picks the message.
func rootError(location Location, err error) error {
	if skills.TargetEscapesBase(location.Root, location.Name) {
		return newError(CodeEscape, location.URI, "path resolves outside the scheme root through a symlink", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return newError(CodeNotFound, location.URI, "not found", err)
	}
	return newError(CodeReadFailed, location.URI, "read failed", err)
}

// listDir lists a directory through the root. Entries that resolve outside
// it are omitted.
func listDir(ctx context.Context, root *os.Root, location Location) (Resource, error) {
	dirEntries, err := fs.ReadDir(root.FS(), filepath.ToSlash(location.Name))
	if err != nil {
		return Resource{}, newError(CodeReadFailed, location.URI, "cannot list the directory", err)
	}
	sort.Slice(dirEntries, func(i, j int) bool {
		return strings.ToLower(dirEntries[i].Name()) < strings.ToLower(dirEntries[j].Name())
	})
	resource := Resource{URI: location.URI, Dir: true}
	for _, entry := range dirEntries {
		if err := ctx.Err(); err != nil {
			return Resource{}, err
		}
		info, err := root.Stat(filepath.Join(location.Name, entry.Name()))
		if err != nil {
			continue
		}
		resource.Total++
		if len(resource.Entries) < MaxEntries {
			resource.Entries = append(resource.Entries, Entry{Name: entry.Name(), Dir: info.IsDir(), Size: info.Size()})
		}
	}
	return resource, nil
}

// HostPath maps a skill:// or local:// URL to an absolute host path for a
// shell command. skill://name is the skill's directory here, since a
// command usually wants to reach files beside SKILL.md. The deepest existing
// ancestor must resolve inside the scheme root through symlinks, and a
// dangling symlink is refused, so a command cannot be handed a path that
// writes outside the root. A missing tail is allowed: commands create files.
func (r *Resolver) HostPath(raw string) (string, error) {
	ref, err := Parse(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme != SchemeSkill && ref.Scheme != SchemeLocal {
		return "", newError(CodeUnsupported, ref.String(), "only skill:// and local:// expand in bash commands", nil)
	}
	if ref.Scheme == SchemeSkill && ref.Path == "" {
		ref.dir = true
	}
	location, err := r.Locate(ref)
	if err != nil {
		return "", err
	}
	if err := r.prepareRoot(ref, location); err != nil {
		return "", err
	}
	if err := checkContained(location); err != nil {
		return "", err
	}
	return location.Abs(), nil
}

// checkContained resolves the deepest existing ancestor of location and
// requires it inside the evaluated scheme root.
func checkContained(location Location) error {
	evaluatedRoot, err := filepath.EvalSymlinks(location.Root)
	if err != nil {
		return newError(CodeNotFound, location.URI, "the scheme directory does not exist", err)
	}
	candidate := location.Abs()
	tail := ""
	for {
		evaluated, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			if !inside(evaluatedRoot, filepath.Join(evaluated, tail)) {
				return newError(CodeEscape, location.URI, "path resolves outside the scheme root through a symlink", nil)
			}
			return nil
		}
		if _, lstatErr := os.Lstat(candidate); lstatErr == nil {
			return newError(CodeEscape, location.URI, "path goes through a dangling or unresolvable symlink", err)
		}
		if candidate == location.Root {
			return newError(CodeNotFound, location.URI, "the scheme directory does not exist", err)
		}
		tail = filepath.Join(filepath.Base(candidate), tail)
		candidate = filepath.Dir(candidate)
	}
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// LocalTarget validates a local:// URL as a write target and creates the
// local:// directory. Writes go through an os.Root on Location.Root.
func (r *Resolver) LocalTarget(raw string) (Location, error) {
	ref, err := Parse(raw)
	if err != nil {
		return Location{}, err
	}
	if ref.Scheme != SchemeLocal {
		return Location{}, newError(CodeUnsupported, ref.String(), "only local:// URLs are writable; skill://, job:// and history:// are read-only", nil)
	}
	if ref.Path == "" {
		return Location{}, newError(CodeTarget, ref.String(), "name a file inside local://", nil)
	}
	location, err := r.Locate(ref)
	if err != nil {
		return Location{}, err
	}
	if err := r.prepareRoot(ref, location); err != nil {
		return Location{}, err
	}
	return location, nil
}

// SearchTarget is a grep target: a file-backed Location, or rendered Text.
type SearchTarget struct {
	URI      string
	Location *Location
	// RootURI names Location.Root, so a file at root-relative path p is
	// Child(RootURI, p).
	RootURI string
	Text    string
}

// Search resolves raw for content search. skill:// names the skill
// directory, job://<id> the job's artifact directory, and job:// and
// history:// listings or transcripts are searched as rendered text.
func (r *Resolver) Search(ctx context.Context, raw string) (SearchTarget, error) {
	ref, err := Parse(raw)
	if err != nil {
		return SearchTarget{}, err
	}
	if ref.Scheme == SchemeHistory || (ref.Scheme == SchemeJob && ref.Name == "") {
		resource, err := r.Read(ctx, raw)
		if err != nil {
			return SearchTarget{}, err
		}
		return SearchTarget{URI: resource.URI, Text: string(resource.Data)}, nil
	}
	if ref.Scheme == SchemeSkill && ref.Path == "" {
		ref.dir = true
	}
	location, err := r.Locate(ref)
	if err != nil {
		return SearchTarget{}, err
	}
	if err := r.prepareRoot(ref, location); err != nil {
		return SearchTarget{}, err
	}
	root := Ref{Scheme: ref.Scheme, Name: ref.Name, dir: true}
	return SearchTarget{URI: ref.String(), Location: &location, RootURI: root.String()}, nil
}

// Child returns the URL of name inside the directory URL base, for listing
// and search output.
func Child(base, name string) string {
	ref, err := Parse(base)
	if err != nil {
		return strings.TrimSuffix(base, "/") + "/" + name
	}
	joined := path.Join(ref.Path, filepath.ToSlash(name))
	if joined == "." {
		joined = ""
	}
	ref.Path = joined
	ref.dir = false
	return ref.String()
}
