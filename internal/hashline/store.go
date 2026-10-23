package hashline

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Store bounds, after omp's snapshot store: many paths, a short history per
// path. A version costs a digest plus one bit per line, so the bound is on
// count, not bytes.
const (
	maxStorePaths       = 256
	maxVersionsPerPath  = 4
	revealLineCap       = 40
	revealColumnCap     = 512
	staleContextRadius  = 2
	staleContextRowsCap = 40
)

// Store is one session's hashline state: the file versions read, grep and
// edits displayed, keyed by canonical path, and the named clipboard
// registers that persist across hashline calls. It is safe for concurrent
// use.
type Store struct {
	mu        sync.Mutex
	clock     uint64
	paths     map[string]*history
	registers map[string][]string
}

// history is the recent versions of one path, newest first.
type history struct {
	used     uint64
	versions []*version
}

// version is one observed file content: its full digest (the identity), the
// 4-hex tag shown to the model (an index only), and which lines a tool
// displayed under that tag.
type version struct {
	sum  [sha256.Size]byte
	tag  string
	seen seenSet
}

// seenSet is the set of 1-based displayed lines; all marks a version whose
// every line is known (whole-file reads, content the model wrote).
type seenSet struct {
	all  bool
	bits []uint64
}

func (s *seenSet) add(line int) {
	if s.all || line < 1 {
		return
	}
	word := (line - 1) / 64
	for len(s.bits) <= word {
		s.bits = append(s.bits, 0)
	}
	s.bits[word] |= 1 << uint((line-1)%64)
}

func (s *seenSet) has(line int) bool {
	if s.all {
		return true
	}
	if line < 1 {
		return false
	}
	word := (line - 1) / 64
	return word < len(s.bits) && s.bits[word]&(1<<uint((line-1)%64)) != 0
}

// NewStore returns an empty session store.
func NewStore() *Store {
	return &Store{paths: map[string]*history{}, registers: map[string][]string{}}
}

// Observe records text displayed for path and returns its tag. displayed
// lists the 1-based lines the tool showed verbatim; they are added to any
// lines already shown for the same content.
func (s *Store) Observe(path, text string, displayed []int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.record(path, sumText(text))
	for _, line := range displayed {
		v.seen.add(line)
	}
	return v.tag
}

// ObserveAll records text whose every line the model knows.
func (s *Store) ObserveAll(path, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.record(path, sumText(text))
	v.seen = seenSet{all: true}
	return v.tag
}

// Forget drops every version of path (the file was deleted).
func (s *Store) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paths, path)
}

// Written records content the model wrote whole (the write tool) and returns
// its tag and a numbered preview of the lines that differ from before.
func (s *Store) Written(path, before, after string) (tag, preview string) {
	oldLines, newLines := Lines(before), Lines(after)
	rows, _ := formatPreview(newLines, lineOrigins(oldLines, newLines), len(oldLines))
	return s.ObserveAll(path, after), rows
}

// Edited records an edit that turned before into after (the edit tool) and
// returns the new tag and a numbered preview of the changed region.
func (s *Store) Edited(path, before, after string) (tag, preview string) {
	oldLines, newLines := Lines(before), Lines(after)
	return s.derive(path, before, after, newLines, lineOrigins(oldLines, newLines), len(oldLines))
}

// derive records after as the successor of before. Lines carried over keep
// their seen state; inserted lines are known to the model; when before was
// never recorded with provenance, every line is known.
func (s *Store) derive(path, before, after string, newLines []string, origins []int, oldCount int) (string, string) {
	rows, displayed := formatPreview(newLines, origins, oldCount)
	s.mu.Lock()
	defer s.mu.Unlock()
	var old *seenSet
	if h := s.paths[path]; h != nil {
		sum := sumText(before)
		for _, v := range h.versions {
			if v.sum == sum {
				old = &v.seen
				break
			}
		}
	}
	var seen seenSet
	if old == nil || old.all {
		seen.all = true
	} else {
		for i, origin := range origins {
			if origin == 0 || origin > 0 && old.has(origin) {
				seen.add(i + 1)
			}
		}
	}
	for _, line := range displayed {
		seen.add(line)
	}
	v := s.record(path, sumText(after))
	v.seen = seen
	return v.tag, rows
}

// Verify checks that tag names the live content text of the file stored
// under key: either a recorded version with exactly this content, or (for
// tags minted before this store existed, e.g. a resumed session) a tag the
// live content hashes to. display names the file in errors.
func (s *Store) Verify(key, display, tag, text string) error {
	_, err := s.matchTag(key, display, tag, text, nil)
	return err
}

// matchTag is Verify returning the matched version, nil when the tag was
// accepted only because the live content hashes to it. named are the lines
// the caller's hunks name, shown as context when the tag is stale.
func (s *Store) matchTag(key, display, tag, text string, named []int) (*version, error) {
	tag = strings.ToUpper(tag)
	sum := sumText(text)
	current := tagOf(sum)
	s.mu.Lock()
	defer s.mu.Unlock()
	known := false
	if h := s.paths[key]; h != nil {
		for _, v := range h.versions {
			if v.tag != tag {
				continue
			}
			known = true
			if v.sum == sum {
				return v, nil
			}
		}
	}
	if !known && current == tag {
		return nil, nil
	}
	// Record the live content with only the rows this error displays, so
	// the tag it names is usable for those rows and nothing else.
	live := s.record(key, sum)
	lines := Lines(text)
	context, shown := anchoredContext(lines, named)
	for _, line := range shown {
		live.seen.add(line)
	}
	return nil, &StaleTagError{Path: display, Tag: tag, Current: current, Known: known, Lines: len(lines), Context: context}
}

// checkSeen enforces the displayed-lines rule on a matched version: every
// touched line must have been displayed under the tag. A nil version (tag
// accepted by content hash alone) carries no provenance and passes.
func (s *Store) checkSeen(v *version, display, tag, text string, touched []int) error {
	if v == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.seen.all {
		return nil
	}
	var unseen []int
	for _, line := range touched {
		if !v.seen.has(line) {
			unseen = append(unseen, line)
		}
	}
	if len(unseen) == 0 {
		return nil
	}
	slices.Sort(unseen)
	unseen = slices.Compact(unseen)
	err := &UnseenLinesError{Path: display, Tag: strings.ToUpper(tag), Lines: unseen}
	if len(unseen) > revealLineCap {
		return err
	}
	lines := Lines(text)
	revealed := make([]string, 0, len(unseen))
	for _, line := range unseen {
		if line > len(lines) || len(lines[line-1]) > revealColumnCap {
			return err
		}
		revealed = append(revealed, fmt.Sprintf("%d:%s", line, lines[line-1]))
	}
	// The error displays these lines in full, so a straight retry with the
	// same tag is an edit of displayed lines.
	for _, line := range unseen {
		v.seen.add(line)
	}
	err.Revealed = revealed
	return err
}

// record returns the version of path with digest sum, creating it (with no
// displayed lines) and evicting the oldest path or version beyond the bounds.
// The caller holds s.mu.
func (s *Store) record(path string, sum [sha256.Size]byte) *version {
	s.clock++
	h := s.paths[path]
	if h == nil {
		if len(s.paths) >= maxStorePaths {
			s.evictOldestPath()
		}
		h = &history{}
		s.paths[path] = h
	}
	h.used = s.clock
	for i, v := range h.versions {
		if v.sum == sum {
			copy(h.versions[1:i+1], h.versions[:i])
			h.versions[0] = v
			return v
		}
	}
	v := &version{sum: sum, tag: tagOf(sum)}
	h.versions = append([]*version{v}, h.versions...)
	if len(h.versions) > maxVersionsPerPath {
		h.versions = h.versions[:maxVersionsPerPath]
	}
	return v
}

func (s *Store) evictOldestPath() {
	oldest := ""
	var used uint64
	for path, h := range s.paths {
		if oldest == "" || h.used < used {
			oldest, used = path, h.used
		}
	}
	delete(s.paths, oldest)
}

// registerSnapshot copies the named registers for one hashline call.
func (s *Store) registerSnapshot() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.registers)
}

// StaleTagError rejects a section whose tag does not name the live file.
type StaleTagError struct {
	Path    string
	Tag     string
	Current string
	// Known reports that the tag was minted this session for content that
	// has since changed; otherwise the tag was never seen for this path.
	Known   bool
	Lines   int
	Context []string
}

func (e *StaleTagError) Error() string {
	var b strings.Builder
	if e.Known {
		fmt.Fprintf(&b, "Edit rejected for %s: the file changed after #%s was read; it now hashes to #%s. ", e.Path, e.Tag, e.Current)
		b.WriteString("If an earlier edit in this session changed it, use the [path#TAG] header and line numbers from that edit's result; otherwise re-read the lines you need and retry with the new tag.")
	} else {
		fmt.Fprintf(&b, "Edit rejected for %s: #%s is not a tag of this file from this session; the file hashes to #%s. ", e.Path, e.Tag, e.Current)
		b.WriteString("Copy the [path#TAG] header from your latest read or grep output (never invent a tag), re-reading the file if needed.")
	}
	if len(e.Context) > 0 {
		fmt.Fprintf(&b, "\nCurrent content around the lines you named (* marks them; %d lines total); these rows count as read under #%s:\n%s", e.Lines, e.Current, strings.Join(e.Context, "\n"))
	}
	return b.String()
}

// UnseenLinesError rejects hunks that touch lines never displayed under the
// section's tag.
type UnseenLinesError struct {
	Path  string
	Tag   string
	Lines []int
	// Revealed holds the touched lines as LINE:TEXT when the error shows them
	// in full; they then count as displayed under Tag.
	Revealed []string
}

func (e *UnseenLinesError) Error() string {
	ranges := formatLineRanges(e.Lines)
	head := fmt.Sprintf("Edit rejected for %s: it touches lines %s, which [%s#%s] never displayed (a partial read, an outline, or search hits). Edits may only touch lines you have read.", e.Path, ranges, e.Path, e.Tag)
	if len(e.Revealed) == 0 {
		return head + fmt.Sprintf(" Read them first, e.g. read path %q, then retry with the tag that read prints.", fmt.Sprintf("%s:%d-%d", e.Path, e.Lines[0], e.Lines[len(e.Lines)-1]))
	}
	return head + "\nTheir current content:\n" + strings.Join(e.Revealed, "\n") +
		fmt.Sprintf("\nIf these are the lines you meant, retry the same patch with [%s#%s]; it now succeeds. If not, fix the line numbers.", e.Path, e.Tag)
}

// anchoredContext renders LINE:TEXT rows within staleContextRadius of each
// anchor, marking anchors with '*', and returns the displayed line numbers.
func anchoredContext(lines []string, anchors []int) ([]string, []int) {
	var show []int
	for _, anchor := range anchors {
		for line := max(1, anchor-staleContextRadius); line <= min(len(lines), anchor+staleContextRadius); line++ {
			if !slices.Contains(show, line) {
				show = append(show, line)
			}
		}
	}
	slices.Sort(show)
	if len(show) > staleContextRowsCap {
		show = show[:staleContextRowsCap]
	}
	rows := make([]string, 0, len(show))
	for i, line := range show {
		if i > 0 && line > show[i-1]+1 {
			rows = append(rows, "...")
		}
		marker := " "
		if slices.Contains(anchors, line) {
			marker = "*"
		}
		rows = append(rows, fmt.Sprintf("%s%d:%s", marker, line, lines[line-1]))
	}
	return rows, show
}

// formatLineRanges compresses sorted lines into "1-4, 7, 10-12".
func formatLineRanges(lines []int) string {
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, fmt.Sprint(lines[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
