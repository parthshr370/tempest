package hashline

import (
	"fmt"
	"strings"
)

const (
	// previewRowsCap bounds the numbered preview an edit result carries.
	previewRowsCap = 60
	// previewRunEdge is how many rows of a long changed run stay on each
	// side of its elision when the full preview would exceed previewRowsCap.
	previewRunEdge = 2
	// lcsCellsCap bounds the line diff behind edit/write previews; a larger
	// changed middle is reported as replaced wholesale.
	lcsCellsCap = 2_000_000
)

// Origin values for lines of a new text.
const (
	// originInserted marks a line the edit wrote.
	originInserted = 0
	// originUnknown marks a line the diff could not attribute.
	originUnknown = -1
)

// lineOrigins maps each new line to the 1-based old line it was carried from,
// originInserted for written lines, or originUnknown when the changed middle
// is too large to diff.
func lineOrigins(oldLines, newLines []string) []int {
	origins := make([]int, len(newLines))
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		origins[prefix] = prefix + 1
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		origins[len(newLines)-1-suffix] = len(oldLines) - suffix
		suffix++
	}
	a := oldLines[prefix : len(oldLines)-suffix]
	b := newLines[prefix : len(newLines)-suffix]
	if len(b) == 0 {
		return origins
	}
	if len(a) == 0 {
		return origins // middle entries are already originInserted
	}
	if len(a)*len(b) > lcsCellsCap {
		for j := range b {
			origins[prefix+j] = originUnknown
		}
		return origins
	}
	// Longest common subsequence over the middle, suffix-indexed.
	width := len(b) + 1
	table := make([]int32, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i*width+j] = table[(i+1)*width+j+1] + 1
			} else {
				table[i*width+j] = max(table[(i+1)*width+j], table[i*width+j+1])
			}
		}
	}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			origins[prefix+j] = prefix + i + 1
			i++
			j++
		case table[(i+1)*width+j] >= table[i*width+j+1]:
			i++
		default:
			j++
		}
	}
	return origins
}

// formatPreview renders the changed regions of newLines as LINE:TEXT rows
// with one line of context and "..." between regions. Changed runs are shown
// in full so the caller can verify the edit without re-reading; only when that
// exceeds previewRowsCap are long runs elided to their edges. oldCount is the
// old text's line count, so a deletion at the end still shows its boundary.
// It returns the rows and the lines they display.
func formatPreview(newLines []string, origins []int, oldCount int) (string, []int) {
	changed := make([]bool, len(newLines))
	want := make([]bool, len(newLines))
	mark := func(line int) { // 0-based, with one line of context
		for k := max(0, line-1); k <= min(len(newLines)-1, line+1); k++ {
			want[k] = true
		}
	}
	lastKept := 0
	for i, origin := range origins {
		if origin <= 0 {
			changed[i] = true
			mark(i)
			continue
		}
		if origin != lastKept+1 && (i == 0 || origins[i-1] > 0) {
			// Old lines between two kept lines were removed with nothing
			// written in their place: show both sides of the cut.
			want[i] = true
			if i > 0 {
				want[i-1] = true
			}
		}
		lastKept = origin
	}
	if len(newLines) > 0 && lastKept < oldCount && origins[len(origins)-1] > 0 {
		want[len(newLines)-1] = true
	}
	plan := planPreview(want, changed, false)
	if len(plan) > previewRowsCap {
		plan = planPreview(want, changed, true)
	}
	var rows []string
	var displayed []int
	for n, i := range plan {
		if n == previewRowsCap {
			rows = append(rows, "... (preview truncated; read the file for the rest)")
			break
		}
		if i < 0 {
			rows = append(rows, "...")
			continue
		}
		rows = append(rows, fmt.Sprintf("%d:%s", i+1, newLines[i]))
		displayed = append(displayed, i+1)
	}
	return strings.Join(rows, "\n"), displayed
}

// planPreview lists the line indexes to display, with -1 for an elision.
// With elideRuns, changed runs longer than 2*previewRunEdge+1 keep only
// previewRunEdge rows at each end.
func planPreview(want, changed []bool, elideRuns bool) []int {
	var plan []int
	for i := 0; i < len(want); {
		if !want[i] {
			i++
			continue
		}
		if len(plan) > 0 && plan[len(plan)-1] >= 0 && i > plan[len(plan)-1]+1 {
			plan = append(plan, -1)
		}
		end := i
		for end+1 < len(want) && changed[i] && changed[end+1] {
			end++
		}
		if elideRuns && end-i+1 > 2*previewRunEdge+1 {
			for k := range previewRunEdge {
				plan = append(plan, i+k)
			}
			plan = append(plan, -1)
			for k := end - previewRunEdge + 1; k <= end; k++ {
				plan = append(plan, k)
			}
			i = end + 1
			continue
		}
		plan = append(plan, i)
		i++
	}
	return plan
}
