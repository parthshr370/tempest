package hashline

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Patch is a parsed hashline input: one section per file, in patch order.
type Patch struct {
	Sections []*Section
	// Warnings are tolerated deviations from the grammar.
	Warnings []string
}

// Section is one `[PATH#TAG]` header and its ops.
type Section struct {
	Path string
	Tag  string
	// Line is the header's 1-based line in the patch.
	Line  int
	Hunks []*Hunk
	// Remove is set by `REM`; Dest by `MV DEST`.
	Remove bool
	Dest   string
}

// locKind is the shape of a hunk locator.
type locKind int

const (
	locRange      locKind = iota // N.=M
	locBlock                     // N*
	locBefore                    // <N, N > 1
	locHead                      // <1
	locAfter                     // >N
	locAfterBlock                // >N*
	locTail                      // >$
)

// Hunk is one PUT or CUT. Start/End are original 1-based lines (End only
// for ranges); Register is a named register, empty for the anonymous one.
type Hunk struct {
	Line     int
	Header   string
	Cut      bool
	Loc      locKind
	Start    int
	End      int
	Colon    bool
	Register string
	Body     []string
}

func (h *Hunk) isGap() bool {
	switch h.Loc {
	case locBefore, locHead, locAfter, locAfterBlock, locTail:
		return true
	}
	return false
}

// pastes reports a register-backed PUT: a named register, or a colonless
// gap PUT reading the anonymous register.
func (h *Hunk) pastes() bool {
	return !h.Cut && (h.Register != "" || !h.Colon)
}

const (
	beginPatchMarker = "*** Begin Patch"
	endPatchMarker   = "*** End Patch"
	registerNameMax  = 64
)

// Parse parses hashline patch text. It checks the grammar and the shape of
// every hunk; line numbers are checked against files later.
func Parse(input string) (*Patch, error) {
	patch := &Patch{}
	var section *Section
	var open *Hunk // last hunk of the current section, nil after a file op
	deferredBlanks := 0
	blankWarned := false
	finish := func() error {
		deferredBlanks = 0
		if open == nil {
			return nil
		}
		h := open
		open = nil
		return checkHunk(h)
	}
	rows := strings.Split(normalizeText(input), "\n")
	for index, raw := range rows {
		lineNum := index + 1
		trimmed := strings.TrimSpace(raw)
		if open != nil && strings.HasPrefix(raw, "+") {
			if !open.Colon || open.Cut || open.Register != "" {
				return nil, fmt.Errorf("line %d: %s", lineNum, bodylessMessage(open))
			}
			for range deferredBlanks {
				open.Body = append(open.Body, "")
			}
			if deferredBlanks > 0 && !blankWarned {
				blankWarned = true
				patch.Warnings = append(patch.Warnings, "Kept blank line(s) between `+` rows as blank body rows. Write a blank body row as a lone `+`.")
			}
			deferredBlanks = 0
			open.Body = append(open.Body, raw[1:])
			continue
		}
		if trimmed == "" {
			if open != nil && len(open.Body) > 0 {
				deferredBlanks++
			}
			continue
		}
		if trimmed == beginPatchMarker {
			continue
		}
		keyword, rest, _ := strings.Cut(trimmed, " ")
		structural := trimmed == endPatchMarker || strings.HasPrefix(trimmed, "[") ||
			keyword == "REM" || keyword == "MV" || keyword == "PUT" || keyword == "CUT"
		if !structural {
			if section == nil {
				return nil, fmt.Errorf("line %d: expected a [PATH#TAG] section header first (copy it from read or grep output); got %q", lineNum, raw)
			}
			return nil, fmt.Errorf("line %d: %s", lineNum, strayRowMessage(raw, trimmed))
		}
		if err := finish(); err != nil {
			return nil, err
		}
		if trimmed == endPatchMarker {
			break
		}
		if strings.HasPrefix(trimmed, "[") {
			next, err := parseSectionHeader(trimmed, lineNum)
			if err != nil {
				return nil, err
			}
			if err := checkSection(section); err != nil {
				return nil, err
			}
			section = next
			patch.Sections = append(patch.Sections, section)
			continue
		}
		if section == nil {
			return nil, fmt.Errorf("line %d: expected a [PATH#TAG] section header first (copy it from read or grep output); got %q", lineNum, raw)
		}
		switch keyword {
		case "REM":
			if strings.TrimSpace(rest) != "" {
				return nil, fmt.Errorf("line %d: `REM` takes nothing after it; got %q", lineNum, raw)
			}
			if section.Remove || section.Dest != "" {
				return nil, fmt.Errorf("line %d: only one file op (`REM` or `MV`) per section", lineNum)
			}
			section.Remove = true
		case "MV":
			dest, err := parseMoveDest(rest, lineNum)
			if err != nil {
				return nil, err
			}
			if section.Remove || section.Dest != "" {
				return nil, fmt.Errorf("line %d: only one file op (`REM` or `MV`) per section", lineNum)
			}
			section.Dest = dest
		default: // PUT or CUT
			h, warning, err := parseHunkHeader(trimmed, lineNum)
			if err != nil {
				return nil, err
			}
			if warning != "" && !slices.Contains(patch.Warnings, warning) {
				patch.Warnings = append(patch.Warnings, warning)
			}
			section.Hunks = append(section.Hunks, h)
			open = h
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if err := checkSection(section); err != nil {
		return nil, err
	}
	if len(patch.Sections) == 0 {
		return nil, fmt.Errorf("patch has no sections; start with a [PATH#TAG] header from read or grep output, then PUT/CUT/REM/MV ops")
	}
	return patch, nil
}

func checkSection(section *Section) error {
	if section == nil {
		return nil
	}
	header := fmt.Sprintf("[%s#%s]", section.Path, section.Tag)
	if section.Remove && len(section.Hunks) > 0 {
		return fmt.Errorf("line %d: `REM` deletes the whole file and cannot be combined with PUT/CUT ops in %s; issue it alone under the header", section.Line, header)
	}
	if !section.Remove && section.Dest == "" && len(section.Hunks) == 0 {
		return fmt.Errorf("line %d: section %s has no ops; add PUT/CUT/REM/MV lines under it", section.Line, header)
	}
	return nil
}

func parseSectionHeader(trimmed string, lineNum int) (*Section, error) {
	if !strings.HasSuffix(trimmed, "]") {
		return nil, fmt.Errorf("line %d: section header must be [PATH#TAG] on its own line; got %q", lineNum, trimmed)
	}
	inner := trimmed[1 : len(trimmed)-1]
	hash := strings.LastIndexByte(inner, '#')
	if hash < 0 {
		path := strings.TrimSpace(inner)
		return nil, fmt.Errorf("line %d: section [%s] has no snapshot tag; use [%s#TAG] with the 4-hex tag from your latest read or grep of that file. To create a new file, use the write tool", lineNum, path, path)
	}
	path := strings.TrimSpace(inner[:hash])
	tag := strings.TrimSpace(inner[hash+1:])
	if path == "" {
		return nil, fmt.Errorf("line %d: section header %q has an empty path", lineNum, trimmed)
	}
	if !IsTag(tag) {
		return nil, fmt.Errorf("line %d: %q is not a section tag; the tag is the 4 hex digits after # in read/grep headers, e.g. [%s#1A2B]", lineNum, tag, path)
	}
	return &Section{Path: path, Tag: strings.ToUpper(tag), Line: lineNum}, nil
}

func parseMoveDest(rest string, lineNum int) (string, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("line %d: `MV` needs a destination path: `MV new/path.go` (quote paths with spaces)", lineNum)
	}
	if quote := rest[0]; quote == '"' || quote == '\'' {
		var b strings.Builder
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if c == '\\' && i+1 < len(rest) {
				i++
				b.WriteByte(rest[i])
				continue
			}
			if c == quote {
				if strings.TrimSpace(rest[i+1:]) != "" {
					return "", fmt.Errorf("line %d: unexpected text after the quoted `MV` destination: %q", lineNum, rest)
				}
				if b.Len() == 0 {
					return "", fmt.Errorf("line %d: `MV` destination is empty", lineNum)
				}
				return b.String(), nil
			}
			b.WriteByte(c)
		}
		return "", fmt.Errorf("line %d: unterminated quote in `MV` destination: %q", lineNum, rest)
	}
	return rest, nil
}

// headerScanner walks a PUT/CUT header.
type headerScanner struct {
	s   string
	pos int
}

func (sc *headerScanner) skipSpace() {
	for sc.pos < len(sc.s) && (sc.s[sc.pos] == ' ' || sc.s[sc.pos] == '\t') {
		sc.pos++
	}
}

func (sc *headerScanner) peek() byte {
	if sc.pos < len(sc.s) {
		return sc.s[sc.pos]
	}
	return 0
}

// number scans a decimal line number; ok is false when none is present.
func (sc *headerScanner) number() (int, bool, error) {
	start := sc.pos
	for sc.pos < len(sc.s) && sc.s[sc.pos] >= '0' && sc.s[sc.pos] <= '9' {
		sc.pos++
	}
	if sc.pos == start {
		return 0, false, nil
	}
	n, err := strconv.Atoi(sc.s[start:sc.pos])
	if err != nil || n > 1<<30 {
		return 0, false, fmt.Errorf("line number %s is too large", sc.s[start:sc.pos])
	}
	if n == 0 {
		return 0, false, fmt.Errorf("line numbers start at 1; got 0")
	}
	return n, true, nil
}

// rangeSeparator consumes `.=` or a tolerated variant (`-`, `..`, `…`, `=`).
func (sc *headerScanner) rangeSeparator() bool {
	start := sc.pos
	sc.skipSpace()
	for _, sep := range []string{".=", "..", "…", "–", "-", "="} {
		if strings.HasPrefix(sc.s[sc.pos:], sep) {
			sc.pos += len(sep)
			sc.skipSpace()
			return true
		}
	}
	sc.pos = start
	return false
}

func parseHunkHeader(header string, lineNum int) (*Hunk, string, error) {
	h := &Hunk{Line: lineNum, Header: header, Cut: strings.HasPrefix(header, "CUT")}
	sc := &headerScanner{s: header, pos: 3}
	usage := func(detail string) error {
		forms := "`PUT N.=M:`, `PUT N*:`, `PUT <N:`, `PUT >N:`, `PUT >N*:`, `PUT >$:`, register pastes `PUT >N @name`, or `PUT N.=M @name`"
		if h.Cut {
			forms = "`CUT N.=M`, `CUT N*`, optionally followed by ` @name`"
		}
		return fmt.Errorf("line %d: %s in %q. Valid forms: %s", lineNum, detail, header, forms)
	}
	sc.skipSpace()
	switch c := sc.peek(); {
	case !h.Cut && (c == '<' || c == '>'):
		sc.pos++
		sc.skipSpace()
		if c == '>' && sc.peek() == '$' {
			sc.pos++
			h.Loc = locTail
			break
		}
		n, ok, err := sc.number()
		if err != nil {
			return nil, "", usage(err.Error())
		}
		if !ok {
			return nil, "", usage("expected a line number (or `$` after `>`)")
		}
		h.Start = n
		star := sc.peek() == '*'
		if star {
			sc.pos++
		}
		switch {
		case c == '>' && star:
			h.Loc = locAfterBlock
		case c == '>':
			h.Loc = locAfter
		case n == 1:
			h.Loc = locHead // `<1` and `<1*` are the file head
		default:
			h.Loc = locBefore // `<N*` is the same gap as `<N`
		}
	default:
		n, ok, err := sc.number()
		if err != nil {
			return nil, "", usage(err.Error())
		}
		if !ok {
			return nil, "", usage("expected a line number")
		}
		h.Start, h.End, h.Loc = n, n, locRange
		if sc.peek() == '*' {
			sc.pos++
			h.Loc = locBlock
			h.End = 0
		} else if sc.rangeSeparator() {
			m, ok, err := sc.number()
			if err != nil {
				return nil, "", usage(err.Error())
			}
			if !ok {
				return nil, "", usage("expected the range end line after `.=`")
			}
			if m < n {
				return nil, "", fmt.Errorf("line %d: invalid range %d.=%d in %q: the value after `.=` is the absolute last line, not a count. For one line use `%s %d.=%d`; for %d lines starting at %d use `%s %d.=%d`", lineNum, n, m, header, header[:3], n, n, m, n, header[:3], n, n+m-1)
			}
			h.End = m
		}
	}
	sc.skipSpace()
	if sc.peek() == '@' {
		sc.pos++
		start := sc.pos
		for sc.pos < len(sc.s) && isRegisterByte(sc.s[sc.pos]) {
			sc.pos++
		}
		if sc.pos == start || sc.pos-start > registerNameMax {
			return nil, "", usage("register names are 1-64 of [A-Za-z0-9_-] after `@`")
		}
		h.Register = sc.s[start:sc.pos]
		sc.skipSpace()
	}
	if sc.peek() == ':' {
		sc.pos++
		h.Colon = true
		sc.skipSpace()
	}
	if sc.pos != len(sc.s) {
		return nil, "", usage(fmt.Sprintf("unexpected %q", sc.s[sc.pos:]))
	}
	warning := ""
	if h.Cut && h.Colon {
		h.Colon = false
		warning = "Ignored a trailing `:` on `CUT`; CUT takes no body (`CUT N.=M`, `CUT N*`)."
	}
	if !h.Cut && h.Colon && h.Register != "" {
		return nil, "", fmt.Errorf("line %d: %q: `PUT … @name` pastes the register and never takes `:` (the colon promises body rows). Drop the colon (`PUT >40 @name`), or drop `@name` and write `+TEXT` body rows", lineNum, header)
	}
	if !h.Cut && !h.Colon && h.Register == "" && !h.isGap() {
		return nil, "", fmt.Errorf("line %d: %q: a colonless PUT pastes a register, and replacing a range needs a named one (`PUT 5.=9 @name`); the anonymous register pastes only at gaps (`PUT >40`). To write content, add `:` and `+TEXT` body rows", lineNum, header)
	}
	return h, warning, nil
}

func isRegisterByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// checkHunk validates a hunk once its body is complete.
func checkHunk(h *Hunk) error {
	if h.Cut || !h.Colon {
		return nil
	}
	if len(h.Body) > 0 {
		return nil
	}
	if h.isGap() {
		return fmt.Errorf("line %d: %q promises body rows and has none. Write `+TEXT` rows under it, or drop the `:` to paste a register (`PUT >N` anonymous, `PUT >N @name` named)", h.Line, h.Header)
	}
	what := fmt.Sprintf("%d.=%d", h.Start, h.End)
	if h.Loc == locBlock {
		what = fmt.Sprintf("%d*", h.Start)
	}
	return fmt.Errorf("line %d: %q has no body rows. To delete lines use `CUT %s`; to replace them, put the final content under the header as `+TEXT` rows", h.Line, h.Header, what)
}

// bodylessMessage explains a `+` row under an op that takes no body.
func bodylessMessage(h *Hunk) string {
	switch {
	case h.Cut:
		return fmt.Sprintf("%q: `CUT` deletes (and captures) lines and takes no body rows. To write new content use `PUT N.=M:` with `+TEXT` rows", h.Header)
	case h.Register != "":
		return fmt.Sprintf("%q: a register PUT pastes captured lines and takes no `+` rows. To write literal text, drop `@%s` and use `PUT …:` with body rows", h.Header, h.Register)
	default:
		return fmt.Sprintf("%q: `PUT` without `:` pastes the anonymous register and takes no body rows. Add `:` after the locator to write literal content (`PUT >40:` then `+TEXT` rows)", h.Header)
	}
}

// strayRowMessage explains a line that is neither a header, an op nor a
// body row.
func strayRowMessage(raw, trimmed string) string {
	switch {
	case strings.HasPrefix(raw, "+"):
		return fmt.Sprintf("body row %q has no `PUT …:` header above it; body rows follow only headers that end in `:`", raw)
	case strings.HasPrefix(trimmed, "+"):
		return fmt.Sprintf("body row %q must start at column 1 with `+` (indentation goes after the `+`)", raw)
	case strings.HasPrefix(trimmed, "-"):
		return fmt.Sprintf("%q: `-` rows do not exist; the PUT range already removes the old lines and the body holds only final content. For a literal line starting with `-` (a Markdown bullet) write `+- item`", raw)
	case isSnapshotRow(trimmed):
		return fmt.Sprintf("%q looks like a pasted read row. Write a `PUT N.=M:` header naming the lines to replace, then their final content as `+TEXT` rows", raw)
	default:
		return fmt.Sprintf("expected a hunk header (PUT, CUT, REM, MV), a `+TEXT` body row under `PUT …:`, or a [PATH#TAG] section header; got %q. Every body row starts with `+`", raw)
	}
}

func isSnapshotRow(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i < len(s) && s[i] == ':'
}

// Paths lists every path the patch names: section paths, then MV
// destinations, in patch order.
func (p *Patch) Paths() []string {
	var paths []string
	for _, s := range p.Sections {
		paths = append(paths, s.Path)
	}
	for _, s := range p.Sections {
		if s.Dest != "" {
			paths = append(paths, s.Dest)
		}
	}
	return paths
}

// SectionPaths scans patch text for section header paths without parsing it,
// for titles of a call in progress.
func SectionPaths(input string) []string {
	var paths []string
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		inner := line[1 : len(line)-1]
		if hash := strings.LastIndexByte(inner, '#'); hash > 0 {
			paths = append(paths, strings.TrimSpace(inner[:hash]))
		}
	}
	return paths
}
