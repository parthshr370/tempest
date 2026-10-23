package manifest

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAML core tags accepted in resources. Anything else, including custom
// application tags, is rejected rather than guessed at.
const (
	nullTag      = "!!null"
	boolTag      = "!!bool"
	intTag       = "!!int"
	floatTag     = "!!float"
	stringTag    = "!!str"
	timestampTag = "!!timestamp"
	binaryTag    = "!!binary"
	mapTag       = "!!map"
	seqTag       = "!!seq"
	mergeTag     = "!!merge"
)

// nodeKind is the type of a canonical value. Scalars keep their YAML type,
// so the string "1" and the integer 1 never compare equal.
type nodeKind byte

const (
	kindNull nodeKind = iota
	kindBool
	kindInt
	kindFloat
	kindString
	kindTimestamp
	kindBinary
	kindMap
	kindSeq
)

// canonicalTags maps each kind to the tag used when it is displayed.
var canonicalTags = map[nodeKind]string{
	kindNull: nullTag, kindBool: boolTag, kindInt: intTag, kindFloat: floatTag,
	kindString: stringTag, kindTimestamp: timestampTag, kindBinary: binaryTag,
}

// node is a canonical value: mapping keys are unique and sorted, sequences
// keep their order, and scalars hold a canonical spelling of their value.
type node struct {
	kind   nodeKind
	text   string
	keys   []string
	values map[string]*node
	items  []*node
}

// field returns a mapping member.
func (n *node) field(key string) (*node, bool) {
	if n.kind != kindMap {
		return nil, false
	}
	value, ok := n.values[key]
	return value, ok
}

// stringField returns a mapping member that is a string scalar.
func (n *node) stringField(key string) (string, bool) {
	value, ok := n.field(key)
	if !ok || value.kind != kindString {
		return "", false
	}
	return value.text, true
}

// fingerprint is an unambiguous typed encoding used for equality. Every
// string is length-prefixed, so no two distinct values share an encoding.
func (n *node) fingerprint() string {
	var out strings.Builder
	n.writeFingerprint(&out)
	return out.String()
}

func (n *node) writeFingerprint(out *strings.Builder) {
	out.WriteByte(byte('0' + n.kind))
	switch n.kind {
	case kindMap:
		out.WriteString(strconv.Itoa(len(n.keys)))
		out.WriteByte('{')
		for _, key := range n.keys {
			writeLengthPrefixed(out, key)
			n.values[key].writeFingerprint(out)
		}
		out.WriteByte('}')
	case kindSeq:
		out.WriteString(strconv.Itoa(len(n.items)))
		out.WriteByte('[')
		for _, item := range n.items {
			item.writeFingerprint(out)
		}
		out.WriteByte(']')
	default:
		writeLengthPrefixed(out, n.text)
	}
}

func writeLengthPrefixed(out *strings.Builder, text string) {
	out.WriteString(strconv.Itoa(len(text)))
	out.WriteByte(':')
	out.WriteString(text)
}

// converter turns one parsed document into canonical nodes, expanding
// aliases and merge keys under the side's node budget.
type converter struct {
	loader     *sideLoader
	name       string
	document   int
	inProgress map[*yaml.Node]bool
}

func (c *converter) fail(parsed *yaml.Node, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	if parsed != nil && parsed.Line > 0 {
		reason = "line " + strconv.Itoa(parsed.Line) + ": " + reason
	}
	return c.loader.fail(c.name, c.document, "%s", reason)
}

// convert normalizes parsed at the given nesting depth. Alias expansion is
// charged against the same node budget as ordinary nodes, so a small
// document cannot expand without bound.
func (c *converter) convert(parsed *yaml.Node, depth int) (*node, error) {
	if depth > maxNesting {
		return nil, c.fail(parsed, "nesting exceeds %d levels", maxNesting)
	}
	c.loader.nodes++
	if c.loader.nodes > maxSideNodes {
		return nil, c.fail(parsed, "more than %d YAML nodes including alias expansion", maxSideNodes)
	}
	if c.loader.nodes%cancelCheckInterval == 0 {
		if err := c.loader.ctx.Err(); err != nil {
			return nil, err
		}
	}
	switch parsed.Kind {
	case yaml.AliasNode:
		if parsed.Alias == nil {
			return nil, c.fail(parsed, "alias has no anchor")
		}
		return c.convert(parsed.Alias, depth)
	case yaml.ScalarNode:
		return c.convertScalar(parsed)
	case yaml.MappingNode:
		return c.withCycleCheck(parsed, func() (*node, error) { return c.convertMapping(parsed, depth) })
	case yaml.SequenceNode:
		return c.withCycleCheck(parsed, func() (*node, error) { return c.convertSequence(parsed, depth) })
	default:
		return nil, c.fail(parsed, "unsupported YAML node")
	}
}

// withCycleCheck refuses an alias that refers back into a collection that
// is still being converted.
func (c *converter) withCycleCheck(parsed *yaml.Node, convert func() (*node, error)) (*node, error) {
	if c.inProgress[parsed] {
		return nil, c.fail(parsed, "cyclic alias")
	}
	c.inProgress[parsed] = true
	defer delete(c.inProgress, parsed)
	return convert()
}

func (c *converter) convertSequence(parsed *yaml.Node, depth int) (*node, error) {
	if tag := parsed.ShortTag(); tag != seqTag {
		return nil, c.fail(parsed, "unsupported tag %s", displayTag(tag))
	}
	items := make([]*node, 0, len(parsed.Content))
	for _, child := range parsed.Content {
		item, err := c.convert(child, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return &node{kind: kindSeq, items: items}, nil
}

// convertMapping requires unique string keys. A single merge key (<<) adds
// the members of the referenced mapping(s) that are not set explicitly;
// earlier merge sources win over later ones, as in YAML's merge type.
func (c *converter) convertMapping(parsed *yaml.Node, depth int) (*node, error) {
	if tag := parsed.ShortTag(); tag != mapTag {
		return nil, c.fail(parsed, "unsupported tag %s", displayTag(tag))
	}
	result := &node{kind: kindMap, values: map[string]*node{}}
	var mergeSource *yaml.Node
	for i := 0; i+1 < len(parsed.Content); i += 2 {
		keyNode := resolveAlias(parsed.Content[i])
		if keyNode.Kind != yaml.ScalarNode {
			return nil, c.fail(parsed.Content[i], "mapping keys must be strings")
		}
		switch keyNode.ShortTag() {
		case mergeTag:
			if mergeSource != nil {
				return nil, c.fail(parsed.Content[i], "duplicate merge key")
			}
			mergeSource = parsed.Content[i+1]
			continue
		case stringTag:
		default:
			return nil, c.fail(parsed.Content[i], "mapping keys must be strings")
		}
		key := keyNode.Value
		if _, duplicate := result.values[key]; duplicate {
			return nil, c.fail(parsed.Content[i], "duplicate mapping key")
		}
		value, err := c.convert(parsed.Content[i+1], depth+1)
		if err != nil {
			return nil, err
		}
		result.values[key] = value
	}
	if mergeSource != nil {
		if err := c.applyMerge(result, mergeSource, depth); err != nil {
			return nil, err
		}
	}
	result.keys = make([]string, 0, len(result.values))
	for key := range result.values {
		result.keys = append(result.keys, key)
	}
	sort.Strings(result.keys)
	return result, nil
}

func (c *converter) applyMerge(target *node, source *yaml.Node, depth int) error {
	resolved := resolveAlias(source)
	var sources []*yaml.Node
	switch resolved.Kind {
	case yaml.MappingNode:
		sources = []*yaml.Node{source}
	case yaml.SequenceNode:
		sources = resolved.Content
	default:
		return c.fail(source, "merge value must be a mapping or a sequence of mappings")
	}
	for _, candidate := range sources {
		if resolveAlias(candidate).Kind != yaml.MappingNode {
			return c.fail(candidate, "merge value must be a mapping or a sequence of mappings")
		}
		merged, err := c.convert(candidate, depth+1)
		if err != nil {
			return err
		}
		for _, key := range merged.keys {
			if _, explicit := target.values[key]; !explicit {
				target.values[key] = merged.values[key]
			}
		}
	}
	return nil
}

// convertScalar keeps the YAML type of a scalar and canonicalizes only its
// spelling: integers in any base become exact decimal (never via float64),
// floats become their shortest round-trip form, and strings stay verbatim.
func (c *converter) convertScalar(parsed *yaml.Node) (*node, error) {
	value := parsed.Value
	explicitlyTagged := parsed.Style&yaml.TaggedStyle != 0
	switch tag := parsed.ShortTag(); tag {
	case nullTag:
		return &node{kind: kindNull, text: "null"}, nil
	case boolTag:
		truth, ok := parseBool(value)
		if !ok {
			return nil, c.fail(parsed, "invalid boolean")
		}
		return &node{kind: kindBool, text: strconv.FormatBool(truth)}, nil
	case intTag:
		integer, ok := parseInteger(value)
		if !ok {
			return nil, c.fail(parsed, "invalid integer")
		}
		return &node{kind: kindInt, text: integer}, nil
	case floatTag:
		// yaml.v3 resolves an integer too large for int64 as a float. Keep
		// it an exact integer unless the author tagged it as a float.
		if !explicitlyTagged {
			if integer, ok := parseInteger(value); ok {
				return &node{kind: kindInt, text: integer}, nil
			}
		}
		number, err := strconv.ParseFloat(strings.ReplaceAll(value, "_", ""), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, c.fail(parsed, "numbers must be finite")
		}
		return &node{kind: kindFloat, text: formatFloat(number)}, nil
	case stringTag:
		return &node{kind: kindString, text: value}, nil
	case timestampTag:
		return &node{kind: kindTimestamp, text: value}, nil
	case binaryTag:
		return &node{kind: kindBinary, text: value}, nil
	default:
		return nil, c.fail(parsed, "unsupported tag %s", displayTag(tag))
	}
}

func resolveAlias(parsed *yaml.Node) *yaml.Node {
	for parsed.Kind == yaml.AliasNode && parsed.Alias != nil {
		parsed = parsed.Alias
	}
	return parsed
}

// parseBool accepts the YAML 1.1 spellings an explicit !!bool tag permits.
func parseBool(value string) (bool, bool) {
	switch strings.ToLower(value) {
	case "true", "yes", "on", "y":
		return true, true
	case "false", "no", "off", "n":
		return false, true
	}
	return false, false
}

// parseInteger parses the integer spellings yaml.v3 resolves (decimal,
// 0x/0o/0b prefixes, a leading 0 for octal, underscores) exactly.
func parseInteger(value string) (string, bool) {
	var integer big.Int
	if _, ok := integer.SetString(value, 0); !ok {
		return "", false
	}
	return integer.String(), true
}

// formatFloat spells a float so it still reads back as a float.
func formatFloat(number float64) string {
	text := strconv.FormatFloat(number, 'g', -1, 64)
	if !strings.ContainsAny(text, ".e") {
		text += ".0"
	}
	return text
}

func displayTag(tag string) string {
	if len(tag) > maxTagDisplay {
		return strconv.Quote(tag[:maxTagDisplay]) + "..."
	}
	return strconv.Quote(tag)
}

// redactedValue replaces each Secret data value in displayed patches.
const redactedValue = "<redacted>"

// secretDataFields are the Secret members whose values are redacted.
var secretDataFields = []string{"data", "stringData"}

// isCoreSecret reports whether identity names a core v1 Secret.
func isCoreSecret(identity Identity) bool {
	return identity.APIVersion == "v1" && identity.Kind == "Secret"
}

// secretDataEqual compares a Secret's actual data and stringData, so a
// change confined to redacted values is still detected.
func secretDataEqual(before, after *node) bool {
	for _, field := range secretDataFields {
		beforeValue, beforeOK := before.field(field)
		afterValue, afterOK := after.field(field)
		if beforeOK != afterOK {
			return false
		}
		if beforeOK && beforeValue.fingerprint() != afterValue.fingerprint() {
			return false
		}
	}
	return true
}

// displayYAML renders a canonical value as indented YAML. With redact set,
// each value under data and stringData is replaced by a fixed marker; the
// keys stay visible, the values, lengths and hashes do not.
func displayYAML(content *node, redact bool) (string, error) {
	document := toYAMLNode(content, redact, true)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func toYAMLNode(content *node, redact bool, isRoot bool) *yaml.Node {
	switch content.kind {
	case kindMap:
		out := &yaml.Node{Kind: yaml.MappingNode, Tag: mapTag, Content: make([]*yaml.Node, 0, 2*len(content.keys))}
		for _, key := range content.keys {
			value := content.values[key]
			var rendered *yaml.Node
			if redact && isRoot && isSecretDataField(key) {
				rendered = redactedNode(value)
			} else {
				rendered = toYAMLNode(value, redact, false)
			}
			out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: stringTag, Value: key}, rendered)
		}
		return out
	case kindSeq:
		out := &yaml.Node{Kind: yaml.SequenceNode, Tag: seqTag, Content: make([]*yaml.Node, 0, len(content.items))}
		for _, item := range content.items {
			out.Content = append(out.Content, toYAMLNode(item, redact, false))
		}
		return out
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: canonicalTags[content.kind], Value: content.text}
	}
}

// redactedNode hides every value of a data map, or the whole value when
// data is not a mapping.
func redactedNode(value *node) *yaml.Node {
	marker := func() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: stringTag, Value: redactedValue} }
	if value.kind != kindMap {
		return marker()
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: mapTag, Content: make([]*yaml.Node, 0, 2*len(value.keys))}
	for _, key := range value.keys {
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: stringTag, Value: key}, marker())
	}
	return out
}

func isSecretDataField(key string) bool {
	for _, field := range secretDataFields {
		if key == field {
			return true
		}
	}
	return false
}
