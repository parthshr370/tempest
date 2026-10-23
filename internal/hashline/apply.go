package hashline

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// clipboard is the register state threaded through one hashline call in
// patch order, across sections. Named registers start from the session's
// and are published when the call succeeds; the anonymous register lives
// only for the call.
type clipboard struct {
	named   map[string][]string
	anon    []string
	hasAnon bool
	// pending lists unlabeled CUTs since the last unlabeled paste; two or
	// more make the next unlabeled paste ambiguous.
	pending []string
}

func (c *clipboard) cut(h *Hunk, rows []string) {
	rows = slices.Clone(rows)
	if h.Register != "" {
		c.named[h.Register] = rows
		return
	}
	c.anon, c.hasAnon = rows, true
	c.pending = append(c.pending, fmt.Sprintf("line %d `%s`", h.Line, h.Header))
}

func (c *clipboard) paste(h *Hunk) ([]string, error) {
	if h.Register != "" {
		rows, ok := c.named[h.Register]
		if ok {
			return rows, nil
		}
		names := make([]string, 0, len(c.named))
		for name := range c.named {
			names = append(names, "@"+name)
		}
		sort.Strings(names)
		available := "No registers hold anything yet."
		if len(names) > 0 {
			available = "Registers holding content: " + strings.Join(names, ", ") + "."
		}
		return nil, fmt.Errorf("line %d: %q pastes @%s, which is empty: no `CUT … @%s` precedes it in this call and no earlier call captured it. %s Capture it first (`CUT N.=M @%s`), or check the name", h.Line, h.Header, h.Register, h.Register, available, h.Register)
	}
	if len(c.pending) > 1 {
		return nil, fmt.Errorf("line %d: %q is an unlabeled paste, but %d unlabeled CUTs are pending (%s), so which one it means is ambiguous. Label the moves (`CUT … @name` then `PUT … @name`), or keep one unlabeled CUT before each unlabeled paste", h.Line, h.Header, len(c.pending), strings.Join(c.pending, ", "))
	}
	if !c.hasAnon {
		return nil, fmt.Errorf("line %d: %q has nothing to paste: no unlabeled `CUT` precedes it in this call, and the anonymous register never carries across calls. Put `CUT N.=M` or `CUT N*` above it, use a named register (`CUT … @name`, `PUT … @name`) for moves across calls, or add `:` and `+TEXT` rows to write content", h.Line, h.Header)
	}
	c.pending = nil
	return c.anon, nil
}

// insertion is rows landing in gap (between original lines gap and gap+1).
type insertion struct {
	gap  int
	rows []string
	// replace marks the body of a range starting at gap+1; it lands after
	// the other insertions in its gap.
	replace bool
}

// applied is one section's edited lines.
type applied struct {
	lines []string
	// origins[i] is the original line new line i+1 came from, 0 when written.
	origins []int
	// notes report how block locators resolved.
	notes []string
	// touched are the original lines the hunks delete or anchor on.
	touched []int
}

// namedLines lists the line numbers a section's hunks name, for context in
// stale-tag errors.
func namedLines(sec *Section) []int {
	var lines []int
	for _, h := range sec.Hunks {
		if h.Start > 0 {
			lines = append(lines, h.Start)
		}
		if h.End > h.Start {
			lines = append(lines, h.End)
		}
	}
	return lines
}

// applySection applies a section's hunks to text. Line numbers name the
// original text: no hunk shifts another's numbers.
func applySection(sec *Section, text string, clip *clipboard) (*applied, error) {
	lines := Lines(text)
	n := len(lines)
	owner := make([]*Hunk, n+2) // owner[l]: the hunk deleting line l
	var inserts []insertion
	// gapInsert is an insertion anchored on a line, checked once every
	// hunk has claimed its deletions.
	type gapInsert struct {
		h   *Hunk
		gap int
	}
	var anchored []gapInsert
	result := &applied{}

	var res resolver
	resolverReady, resolverOK := false, false
	block := func(h *Hunk) (int, error) {
		if !resolverReady {
			res, resolverOK = newResolver(sec.Path, text, lines)
			resolverReady = true
		}
		if !resolverOK {
			kind := filepath.Ext(sec.Path)
			if kind == "" {
				kind = "files without an extension"
			} else {
				kind += " files"
			}
			return 0, fmt.Errorf("line %d: %q needs a block resolver, and %s have none (block locators support %s). Use %s with explicit lines", h.Line, h.Header, kind, blockLanguages, plainForm(h))
		}
		end, problem := res(h.Start)
		if problem != "" {
			return 0, blockFailure(h, problem, lines, res)
		}
		if end <= h.Start {
			msg := fmt.Sprintf("line %d: %q resolved a single line: line %d is one statement, not the opening line of a multi-line construct. For this line use %s", h.Line, h.Header, h.Start, singleForm(h))
			if hint := enclosingHint(h, res); hint != "" {
				msg += ". " + hint
			}
			return 0, fmt.Errorf("%s", msg)
		}
		result.notes = append(result.notes, fmt.Sprintf("%s -> lines %d-%d", h.Header, h.Start, end))
		return end, nil
	}
	checkLine := func(h *Hunk, line int) error {
		if line > n {
			if n == 0 {
				return fmt.Errorf("line %d: %q names line %d, but %s is empty; only `PUT <1:` and `PUT >$:` apply to an empty file", h.Line, h.Header, line, sec.Path)
			}
			return fmt.Errorf("line %d: %q names line %d, but %s has %d lines. Take line numbers from your latest read of the file", h.Line, h.Header, line, sec.Path, n)
		}
		return nil
	}
	claim := func(h *Hunk, start, end int) error {
		for line := start; line <= end; line++ {
			if other := owner[line]; other != nil {
				return fmt.Errorf("line %d: %q touches line %d, which the hunk on line %d (%q) already targets. Hunks may not overlap: issue one hunk per range, its body the final content", h.Line, h.Header, line, other.Line, other.Header)
			}
		}
		for line := start; line <= end; line++ {
			owner[line] = h
			result.touched = append(result.touched, line)
		}
		return nil
	}
	rowsFor := func(h *Hunk) ([]string, error) {
		if h.pastes() {
			return clip.paste(h)
		}
		return h.Body, nil
	}

	for _, h := range sec.Hunks {
		switch h.Loc {
		case locRange, locBlock:
			if err := checkLine(h, h.Start); err != nil {
				return nil, err
			}
			end := h.End
			if h.Loc == locBlock {
				var err error
				if end, err = block(h); err != nil {
					return nil, err
				}
			} else if err := checkLine(h, end); err != nil {
				return nil, err
			}
			if err := claim(h, h.Start, end); err != nil {
				return nil, err
			}
			if h.Cut {
				clip.cut(h, lines[h.Start-1:end])
				continue
			}
			rows, err := rowsFor(h)
			if err != nil {
				return nil, err
			}
			inserts = append(inserts, insertion{gap: h.Start - 1, rows: rows, replace: true})
		case locHead, locTail:
			rows, err := rowsFor(h)
			if err != nil {
				return nil, err
			}
			gap := 0
			if h.Loc == locTail {
				gap = n
			}
			inserts = append(inserts, insertion{gap: gap, rows: rows})
		case locBefore, locAfter, locAfterBlock:
			if err := checkLine(h, h.Start); err != nil {
				return nil, err
			}
			at := h.Start
			if h.Loc == locAfterBlock {
				var err error
				if at, err = block(h); err != nil {
					return nil, err
				}
				result.touched = append(result.touched, h.Start)
			}
			rows, err := rowsFor(h)
			if err != nil {
				return nil, err
			}
			gap := at
			if h.Loc == locBefore {
				gap = at - 1
			}
			anchored = append(anchored, gapInsert{h, gap})
			result.touched = append(result.touched, at)
			inserts = append(inserts, insertion{gap: gap, rows: rows})
		}
	}
	// A gap strictly inside one hunk's deleted run has no position left
	// once the run is gone; gaps at a run's edges stay well defined.
	for _, a := range anchored {
		if other := owner[a.gap]; other != nil && owner[a.gap+1] == other {
			return nil, fmt.Errorf("line %d: %q inserts between lines %d and %d, both removed by the hunk on line %d (%q). Insert at a line outside that range, or put the new rows in that hunk's body", a.h.Line, a.h.Header, a.gap, a.gap+1, other.Line, other.Header)
		}
	}

	// Stable: within a gap, plain insertions keep patch order and the
	// replacement body of a range starting after the gap comes last.
	sort.SliceStable(inserts, func(i, j int) bool {
		if inserts[i].gap != inserts[j].gap {
			return inserts[i].gap < inserts[j].gap
		}
		return !inserts[i].replace && inserts[j].replace
	})
	next := 0
	for gap := 0; gap <= n; gap++ {
		for ; next < len(inserts) && inserts[next].gap == gap; next++ {
			for _, row := range inserts[next].rows {
				result.lines = append(result.lines, row)
				result.origins = append(result.origins, originInserted)
			}
		}
		if gap < n && owner[gap+1] == nil {
			result.lines = append(result.lines, lines[gap])
			result.origins = append(result.origins, gap+1)
		}
	}
	return result, nil
}

// plainForm is the explicit-line fallback for a block hunk.
func plainForm(h *Hunk) string {
	reg := ""
	if h.Register != "" {
		reg = " @" + h.Register
	}
	switch {
	case h.Cut:
		return fmt.Sprintf("`CUT %d.=M%s`", h.Start, reg)
	case h.Loc == locAfterBlock && h.pastes():
		return fmt.Sprintf("`PUT >M%s` (M = the construct's last line)", reg)
	case h.Loc == locAfterBlock:
		return "`PUT >M:` (M = the construct's last line)"
	case h.Register != "":
		return fmt.Sprintf("`PUT %d.=M%s`", h.Start, reg)
	default:
		return fmt.Sprintf("`PUT %d.=M:`", h.Start)
	}
}

// singleForm is the exact one-line form of a block hunk.
func singleForm(h *Hunk) string {
	reg := ""
	if h.Register != "" {
		reg = " @" + h.Register
	}
	switch {
	case h.Cut:
		return fmt.Sprintf("`CUT %d.=%d%s`", h.Start, h.Start, reg)
	case h.Loc == locAfterBlock && h.pastes():
		return fmt.Sprintf("`PUT >%d%s`", h.Start, reg)
	case h.Loc == locAfterBlock:
		return fmt.Sprintf("`PUT >%d:`", h.Start)
	case h.Register != "":
		return fmt.Sprintf("`PUT %d.=%d%s`", h.Start, h.Start, reg)
	default:
		return fmt.Sprintf("`PUT %d.=%d:`", h.Start, h.Start)
	}
}

// blockForm is the block header of h re-anchored on line.
func blockForm(h *Hunk, line int) string {
	reg := ""
	if h.Register != "" {
		reg = " @" + h.Register
	}
	switch {
	case h.Cut:
		return fmt.Sprintf("`CUT %d*%s`", line, reg)
	case h.Loc == locAfterBlock && h.pastes():
		return fmt.Sprintf("`PUT >%d*%s`", line, reg)
	case h.Loc == locAfterBlock:
		return fmt.Sprintf("`PUT >%d*:`", line)
	case h.Register != "":
		return fmt.Sprintf("`PUT %d*%s`", line, reg)
	default:
		return fmt.Sprintf("`PUT %d*:`", line)
	}
}

// enclosingSearch bounds how far block-error hints look for a construct.
const (
	enclosingSearchLines = 200
	nextBlockSearchLines = 20
)

// enclosingHint names the nearest multi-line construct containing h.Start.
func enclosingHint(h *Hunk, res resolver) string {
	for k := h.Start - 1; k >= max(1, h.Start-enclosingSearchLines); k-- {
		if end, problem := res(k); problem == "" && end > k && end >= h.Start {
			return fmt.Sprintf("The nearest enclosing construct spans lines %d-%d; %s targets it", k, end, blockForm(h, k))
		}
	}
	return ""
}

// blockFailure explains a block locator that resolved no construct.
func blockFailure(h *Hunk, problem string, lines []string, res resolver) error {
	var b strings.Builder
	fmt.Fprintf(&b, "line %d: %q could not resolve a construct opening on line %d: %s.", h.Line, h.Header, h.Start, problem)
	if strings.TrimSpace(lines[h.Start-1]) == "" {
		for k := h.Start + 1; k <= min(len(lines), h.Start+nextBlockSearchLines); k++ {
			if end, p := res(k); p == "" && end > k {
				fmt.Fprintf(&b, " The next construct spans lines %d-%d; retry %s.", k, end, blockForm(h, k))
				break
			}
		}
	}
	if hint := enclosingHint(h, res); hint != "" {
		b.WriteString(" " + hint + ".")
	}
	fmt.Fprintf(&b, " Or use %s with explicit lines.", plainForm(h))
	if rows, _ := anchoredContext(lines, []int{h.Start}); len(rows) > 0 {
		b.WriteString("\n" + strings.Join(rows, "\n"))
	}
	return fmt.Errorf("%s", b.String())
}
