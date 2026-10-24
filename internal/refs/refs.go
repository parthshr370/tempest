// Package refs resolves tempest's internal URLs, stable names the model uses
// for things that do not live in the workspace:
//
//	skill://<name>             a skill's file; skill://<name>/ lists its directory
//	skill://<name>/<path>      a file or directory beside it
//	job://                     the session tree's jobs
//	job://<job id>             one job's record; job://<job id>/ lists its artifacts
//	job://<job id>/<file>      one artifact (output.log, result.txt, changes.patch)
//	history://                 the session tree's child sessions
//	history://<id>             one child's transcript (task job id or child session id)
//	local://[<path>]           the root session's scratch directory, shared with its children
//
// Every scheme is rooted. A path may not contain "..", and file access goes
// through an [os.Root] on the scheme's directory, so a symlink cannot leave
// it. A child session resolves local://, job:// and history:// against its
// root session, which keeps a session tree's names shared and other trees
// out of reach.
package refs

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/work"
)

// The supported schemes.
const (
	SchemeSkill   = "skill"
	SchemeJob     = "job"
	SchemeHistory = "history"
	SchemeLocal   = "local"
	SchemeIssue   = "issue"
	SchemePR      = "pr"
)

// LocalDirName is the local:// directory inside the root session's data
// directory (<journal>.d/local).
const LocalDirName = "local"

// MaxFileBytes bounds one file read through a URL. Larger files stay
// reachable through bash, which streams.
const MaxFileBytes = 64 << 20

// Error codes classify resolution failures.
const (
	CodeInvalid     = "invalid_url"
	CodeUnsupported = "unsupported"
	CodeUnavailable = "unavailable"
	CodeNotFound    = "not_found"
	CodeEscape      = "path_escape"
	CodeTarget      = "invalid_target"
	CodeTooLarge    = "too_large"
	CodeReadFailed  = "read_failed"
)

// Error is a typed resolution failure. Message is written for the model and
// never includes a disk location.
type Error struct {
	Code    string
	URI     string
	Message string
	Err     error
}

func (e *Error) Error() string { return e.URI + ": " + e.Message }

func (e *Error) Unwrap() error { return e.Err }

// Is matches another *Error by code.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code == e.Code
}

func newError(code, uri, message string, err error) *Error {
	return &Error{Code: code, URI: uri, Message: message, Err: err}
}

// Ref is a parsed internal URL.
type Ref struct {
	Scheme string
	// Name is the skill name, job id or history id; empty for local:// and
	// for the bare job:// and history:// listings.
	Name string
	// Path is the slash-separated path inside the scheme root; "" is the
	// root itself.
	Path string
	// dir marks a trailing slash on a named root: skill://name/ is the
	// skill directory and job://id/ the job's artifact directory, while
	// skill://name is the skill file and job://id the job record.
	dir bool
}

// IsDirForm reports a trailing slash after a skill name or job id.
func (r Ref) IsDirForm() bool { return r.dir && r.Path == "" && r.Name != "" }

// String renders the canonical URL, escaping the path so it parses back.
func (r Ref) String() string {
	out := r.Scheme + "://" + r.Name
	if r.IsDirForm() {
		return out + "/"
	}
	if r.Path != "" {
		if r.Name != "" {
			out += "/"
		}
		out += strings.TrimPrefix((&url.URL{Path: "/" + r.Path}).EscapedPath(), "/")
	}
	return out
}

var (
	jobIDPattern   = regexp.MustCompile(`^job_[0-9a-f]{32}$`)
	childIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// IsURL reports whether s starts with one of the supported schemes.
func IsURL(s string) bool {
	scheme, _, ok := strings.Cut(s, "://")
	if !ok {
		return false
	}
	switch scheme {
	case SchemeSkill, SchemeJob, SchemeHistory, SchemeLocal, SchemeIssue, SchemePR:
		return true
	}
	return false
}

// Parse validates raw lexically. It does no I/O.
func Parse(raw string) (Ref, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || !IsURL(raw) {
		return Ref{}, newError(CodeInvalid, raw, "not an internal URL; supported schemes are skill://, job://, history://, local://, issue:// and pr://", nil)
	}
	if GitHubURL(raw) {
		if _, err := parseGitHub(raw); err != nil {
			return Ref{}, newError(CodeInvalid, raw, err.Error(), err)
		}
		return Ref{Scheme: scheme, Name: rest}, nil
	}
	if strings.ContainsAny(rest, "?#") {
		return Ref{}, newError(CodeInvalid, raw, "internal URLs take no query or fragment; percent-encode ? and # in names", nil)
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return Ref{}, newError(CodeInvalid, raw, "invalid percent-encoding", err)
	}
	ref := Ref{Scheme: scheme, dir: strings.HasSuffix(decoded, "/") || strings.HasSuffix(decoded, "/.")}
	if scheme == SchemeLocal {
		if ref.Path, err = cleanRelative(raw, decoded); err != nil {
			return Ref{}, err
		}
		return ref, nil
	}
	name, sub, _ := strings.Cut(decoded, "/")
	ref.Name = name
	switch scheme {
	case SchemeSkill:
		if name == "" {
			return Ref{}, newError(CodeInvalid, raw, "skill:// needs a skill name", nil)
		}
	case SchemeJob:
		if name != "" && !jobIDPattern.MatchString(name) {
			return Ref{}, newError(CodeInvalid, raw, "not a job id (job_ followed by 32 hex digits)", nil)
		}
	case SchemeHistory:
		if name != "" && !jobIDPattern.MatchString(name) && !childIDPattern.MatchString(name) {
			return Ref{}, newError(CodeInvalid, raw, "history:// takes a task job id or a 16-hex child session id", nil)
		}
		if strings.Trim(sub, "/") != "" {
			return Ref{}, newError(CodeInvalid, raw, "history:// has no sub-paths", nil)
		}
		ref.dir = false
		return ref, nil
	}
	if name == "" && sub != "" {
		return Ref{}, newError(CodeInvalid, raw, "missing name before the path", nil)
	}
	if ref.Path, err = cleanRelative(raw, sub); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// cleanRelative accepts only a relative slash path without "..", NUL or
// backslashes, and returns it cleaned ("" for the root).
func cleanRelative(raw, p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return "", newError(CodeInvalid, raw, "path contains a NUL or backslash", nil)
	}
	if strings.HasPrefix(p, "/") {
		return "", newError(CodeInvalid, raw, "path must be relative to the scheme root", nil)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", newError(CodeEscape, raw, "path escapes the scheme root (\"..\" is not allowed)", nil)
		}
	}
	cleaned := path.Clean("/" + p)
	return strings.TrimPrefix(cleaned, "/"), nil
}

// Config scopes a Resolver to one session tree.
type Config struct {
	Skills []skills.Skill
	// RootJournal is the root session's journal path. A child passes its
	// root's journal, never its own. Empty means the session is not
	// journaled: job://, history:// and local:// are unavailable.
	RootJournal string
}

// Resolver resolves internal URLs for one session tree. It is immutable and
// safe for concurrent use.
type Resolver struct {
	skills      *skills.Resolver
	skillNames  []string
	rootJournal string
}

// New validates cfg. Skills are indexed through [skills.NewResolver].
func New(cfg Config) (*Resolver, error) {
	resolver := &Resolver{rootJournal: cfg.RootJournal}
	if cfg.RootJournal != "" && !filepath.IsAbs(cfg.RootJournal) {
		return nil, errors.New("refs: root journal path must be absolute")
	}
	if len(cfg.Skills) > 0 {
		index, err := skills.NewResolver(cfg.Skills)
		if err != nil {
			return nil, err
		}
		resolver.skills = index
		resolver.skillNames = index.SkillNames()
	}
	return resolver, nil
}

// RootDataDir is the root session's data directory, or "" when unjournaled.
func (r *Resolver) RootDataDir() string {
	if r == nil || r.rootJournal == "" {
		return ""
	}
	return r.rootJournal + ".d"
}

// Location is a file-backed URL target: Name inside the directory Root.
// Open it through an os.Root on Root.
type Location struct {
	URI string
	// Root is the scheme root directory, absolute.
	Root string
	// Name is the OS path relative to Root; "." is Root itself.
	Name string
}

// Abs is the target's absolute path, for display to tools that need one.
func (l Location) Abs() string { return filepath.Join(l.Root, l.Name) }

// Locate maps a file-backed URL to its directory and relative name. It
// fails for rendered resources (job records, transcripts) and for local://
// in an unjournaled session. Locate does no symlink checks; open the
// result through an os.Root, or use [Resolver.HostPath].
func (r *Resolver) Locate(ref Ref) (Location, error) {
	uri := ref.String()
	switch ref.Scheme {
	case SchemeSkill:
		if r == nil || r.skills == nil {
			return Location{}, newError(CodeUnavailable, uri, "no skills are loaded in this session", nil)
		}
		location, err := r.skills.Locate(skillURI(ref))
		if err != nil {
			return Location{}, r.skillError(uri, err)
		}
		if ref.Path == "" && ref.dir {
			return Location{URI: uri, Root: location.BaseDir, Name: "."}, nil
		}
		return Location{URI: uri, Root: location.BaseDir, Name: location.Rel}, nil
	case SchemeLocal:
		dir := r.RootDataDir()
		if dir == "" {
			return Location{}, newError(CodeUnavailable, uri, "local:// needs a journaled session", nil)
		}
		return Location{URI: uri, Root: filepath.Join(dir, LocalDirName), Name: osName(ref.Path)}, nil
	case SchemeJob:
		dir := r.RootDataDir()
		if dir == "" {
			return Location{}, newError(CodeUnavailable, uri, "job:// needs a journaled session", nil)
		}
		if ref.Name == "" {
			return Location{}, newError(CodeTarget, uri, "the job list is rendered, not a directory; name a job", nil)
		}
		return Location{URI: uri, Root: filepath.Join(dir, work.JobArtifactsDir, ref.Name), Name: osName(ref.Path)}, nil
	case SchemeIssue, SchemePR:
		return Location{}, newError(CodeTarget, uri, "GitHub references are rendered, not workspace paths; use read for issue:// and pr://", nil)
	default:
		return Location{}, newError(CodeTarget, uri, "history:// is rendered from a journal and has no file path", nil)
	}
}

// skillURI rebuilds the skill:// form the skills resolver parses.
func skillURI(ref Ref) string {
	out := "skill://" + url.PathEscape(ref.Name)
	if ref.Path != "" {
		out += (&url.URL{Path: "/" + ref.Path}).EscapedPath()
	}
	return out
}

func (r *Resolver) skillError(uri string, err error) error {
	var resolveErr *skills.ResolveError
	if !errors.As(err, &resolveErr) {
		return newError(CodeReadFailed, uri, err.Error(), err)
	}
	switch resolveErr.Code {
	case skills.ResolveUnknownSkill:
		return newError(CodeNotFound, uri, fmt.Sprintf("unknown skill %q; available: %s", resolveErr.Skill, strings.Join(r.skillNames, ", ")), fs.ErrNotExist)
	case skills.ResolvePathEscape:
		return newError(CodeEscape, uri, "path escapes the skill directory", err)
	default:
		return newError(CodeInvalid, uri, "invalid skill path", err)
	}
}

func osName(slashPath string) string {
	if slashPath == "" {
		return "."
	}
	return filepath.FromSlash(slashPath)
}
