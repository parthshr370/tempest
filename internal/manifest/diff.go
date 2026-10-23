package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Option bounds accepted by Compare.
const (
	MaxContextLines = 10
	MaxResources    = 200
	// maxPatchBytes bounds each resource's display patch, whole lines only.
	maxPatchBytes = 8 << 10
	// MaxOutputBytes bounds the encoded result as delivered by a tool: the
	// JSON once as text content and once more as structured details.
	MaxOutputBytes = 64 << 10
)

// Change classifications.
const (
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeChanged = "changed"
)

// patchMarker opens every patch: it is a reading aid over normalized YAML,
// so applying it to the source files would be wrong.
const patchMarker = "# display diff of canonical YAML, not an applicable patch\n"

// patchTruncatedMarker ends a patch that was cut at a whole line.
const patchTruncatedMarker = "# patch truncated\n"

// secretOnlyNote explains a changed Secret whose redacted view is equal.
const secretOnlyNote = "# only redacted Secret data values differ\n"

// Options selects patch context, the number of reported resources and
// whether Secret data values are hidden in patches.
type Options struct {
	ContextLines  int
	MaxResources  int
	RedactSecrets bool
}

// Identity is a resource's exact key. An absent namespace is the empty
// string and is distinct from an explicit namespace such as "default".
type Identity struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

func (i Identity) less(other Identity) bool {
	if i.APIVersion != other.APIVersion {
		return i.APIVersion < other.APIVersion
	}
	if i.Kind != other.Kind {
		return i.Kind < other.Kind
	}
	if i.Namespace != other.Namespace {
		return i.Namespace < other.Namespace
	}
	return i.Name < other.Name
}

// label names a resource in errors and patch headers.
func (i Identity) label() string {
	if i.Namespace == "" {
		return fmt.Sprintf("%s %s %s", i.APIVersion, i.Kind, i.Name)
	}
	return fmt.Sprintf("%s %s %s/%s", i.APIVersion, i.Kind, i.Namespace, i.Name)
}

// Source locates a resource: a filesystem path and a one-based document.
type Source struct {
	Path     string `json:"path"`
	Document int    `json:"document"`
}

// Summary counts every resource of the complete input, not just the
// resources listed in a bounded result.
type Summary struct {
	Before    int `json:"before"`
	After     int `json:"after"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
}

// Resource is one added, removed or changed resource.
type Resource struct {
	Identity          Identity `json:"identity"`
	Change            string   `json:"change"`
	BeforeSource      *Source  `json:"before_source,omitempty"`
	AfterSource       *Source  `json:"after_source,omitempty"`
	Patch             string   `json:"patch"`
	PatchTruncated    bool     `json:"patch_truncated"`
	SecretDataChanged bool     `json:"secret_data_changed"`
}

// Result is the bounded comparison. Resources are sorted by identity;
// OmittedResources counts changed resources left out by MaxResources or the
// output budget, and Truncated is set when anything was left out or cut.
type Result struct {
	Summary          Summary    `json:"summary"`
	Resources        []Resource `json:"resources"`
	OmittedResources int        `json:"omitted_resources"`
	Truncated        bool       `json:"truncated"`
}

// JSON encodes the result compactly, leaving <, > and & readable.
func (r Result) JSON() ([]byte, error) {
	return encodeJSON(r)
}

func encodeJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// Compare decodes both sides and classifies every resource by identity and
// canonical content. Pass View to select a file or directory inside a larger
// filesystem. It reads only through the given filesystems and runs nothing.
func Compare(ctx context.Context, before, after fs.FS, options Options) (Result, error) {
	if options.ContextLines < 0 || options.ContextLines > MaxContextLines {
		return Result{}, fmt.Errorf("context_lines must be between 0 and %d", MaxContextLines)
	}
	if options.MaxResources < 1 || options.MaxResources > MaxResources {
		return Result{}, fmt.Errorf("max_resources must be between 1 and %d", MaxResources)
	}
	beforeResources, err := loadSide(ctx, "before", before)
	if err != nil {
		return Result{}, err
	}
	afterResources, err := loadSide(ctx, "after", after)
	if err != nil {
		return Result{}, err
	}
	result := Result{Summary: Summary{Before: len(beforeResources), After: len(afterResources)}, Resources: []Resource{}}
	var differing []Identity
	for _, identity := range sortedIdentities(beforeResources, afterResources) {
		previous, current := beforeResources[identity], afterResources[identity]
		switch {
		case previous == nil:
			result.Summary.Added++
		case current == nil:
			result.Summary.Removed++
		case previous.fingerprint == current.fingerprint:
			result.Summary.Unchanged++
			continue
		default:
			result.Summary.Changed++
		}
		differing = append(differing, identity)
	}
	for index, identity := range differing {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if index == options.MaxResources {
			break
		}
		entry, err := describe(beforeResources[identity], afterResources[identity], options)
		if err != nil {
			return Result{}, err
		}
		result.Resources = append(result.Resources, entry)
	}
	result.OmittedResources = len(differing) - len(result.Resources)
	if err := fitOutputBudget(&result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func sortedIdentities(before, after map[Identity]*resource) []Identity {
	identities := make([]Identity, 0, len(before)+len(after))
	for identity := range before {
		identities = append(identities, identity)
	}
	for identity := range after {
		if _, seen := before[identity]; !seen {
			identities = append(identities, identity)
		}
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].less(identities[j]) })
	return identities
}

// describe renders one differing resource. Either side may be nil.
func describe(previous, current *resource, options Options) (Resource, error) {
	var entry Resource
	var previousText, currentText string
	var err error
	if previous != nil {
		entry.Identity = previous.identity
		entry.BeforeSource = &Source{Path: previous.source.Path, Document: previous.source.Document}
		previousText, err = displayYAML(previous.content, options.RedactSecrets && isCoreSecret(previous.identity))
		if err != nil {
			return Resource{}, err
		}
	}
	if current != nil {
		entry.Identity = current.identity
		entry.AfterSource = &Source{Path: current.source.Path, Document: current.source.Document}
		currentText, err = displayYAML(current.content, options.RedactSecrets && isCoreSecret(current.identity))
		if err != nil {
			return Resource{}, err
		}
	}
	switch {
	case previous == nil:
		entry.Change = ChangeAdded
	case current == nil:
		entry.Change = ChangeRemoved
	default:
		entry.Change = ChangeChanged
		entry.SecretDataChanged = isCoreSecret(entry.Identity) && !secretDataEqual(previous.content, current.content)
	}
	patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        splitLines(previousText),
		B:        splitLines(currentText),
		FromFile: "before " + entry.Identity.label(),
		ToFile:   "after " + entry.Identity.label(),
		Context:  options.ContextLines,
	})
	if err != nil {
		return Resource{}, err
	}
	switch {
	case patch != "":
		entry.Patch, entry.PatchTruncated = boundPatch(patchMarker + patch)
	case entry.SecretDataChanged:
		entry.Patch = patchMarker + secretOnlyNote
	}
	return entry, nil
}

// splitLines splits text after each newline. Unlike difflib.SplitLines it
// adds no empty final line, so a full addition is exactly the resource.
func splitLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// boundPatch keeps whole lines of patch within maxPatchBytes, ending a cut
// patch with a marker. Lines are never split, so UTF-8 stays intact.
func boundPatch(patch string) (string, bool) {
	if len(patch) <= maxPatchBytes {
		return patch, false
	}
	limit := maxPatchBytes - len(patchTruncatedMarker)
	cut := strings.LastIndexByte(patch[:limit], '\n') + 1
	return patch[:cut] + patchTruncatedMarker, true
}

// fitOutputBudget keeps the longest prefix of resources whose result,
// delivered as JSON text plus the same JSON as structured details, fits
// MaxOutputBytes, and sets OmittedResources and Truncated to match. The
// delivered size is additive over the pieces of the encoding because JSON
// string escaping works character by character, so each resource is
// measured once instead of re-encoding the whole result per candidate.
func fitOutputBudget(result *Result) error {
	separatorCost, err := deliveredCost([]byte(","))
	if err != nil {
		return err
	}
	differing := result.OmittedResources + len(result.Resources)
	listed, entriesCost := 0, 0
	for k := 1; k <= len(result.Resources); k++ {
		encoded, err := encodeJSON(result.Resources[k-1])
		if err != nil {
			return err
		}
		cost, err := deliveredCost(encoded)
		if err != nil {
			return err
		}
		entriesCost += cost
		if k > 1 {
			entriesCost += separatorCost
		}
		frame, err := frameCost(result, k, differing)
		if err != nil {
			return err
		}
		if frame+entriesCost > MaxOutputBytes {
			break
		}
		listed = k
	}
	result.Resources = result.Resources[:listed]
	result.OmittedResources = differing - listed
	result.Truncated = result.OmittedResources > 0
	for _, entry := range result.Resources {
		result.Truncated = result.Truncated || entry.PatchTruncated
	}
	return nil
}

// frameCost is the delivered size of the result without its resources,
// given that its first listed resources are kept.
func frameCost(result *Result, listed, differing int) (int, error) {
	frame := Result{Summary: result.Summary, Resources: []Resource{}, OmittedResources: differing - listed}
	frame.Truncated = frame.OmittedResources > 0
	for _, entry := range result.Resources[:listed] {
		frame.Truncated = frame.Truncated || entry.PatchTruncated
	}
	encoded, err := encodeJSON(frame)
	if err != nil {
		return 0, err
	}
	cost, err := deliveredCost(encoded)
	if err != nil {
		return 0, err
	}
	// The text content's two quote characters wrap the whole encoding once.
	return cost + 2, nil
}

// deliveredCost is the size of an encoded piece plus its escaped form
// inside a JSON string, excluding the string's surrounding quotes.
func deliveredCost(encoded []byte) (int, error) {
	quoted, err := json.Marshal(string(encoded))
	if err != nil {
		return 0, err
	}
	return len(encoded) + len(quoted) - 2, nil
}
