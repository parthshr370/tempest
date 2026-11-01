package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
)

// settingsNode is one parsed JSON value of a settings layer. Exactly one shape
// is set: object members, array items, or a compact scalar (string, number,
// boolean or null). Layers are merged as trees so attribution and requirement
// checks can walk dotted paths without re-decoding.
type settingsNode struct {
	object  map[string]*settingsNode
	array   []*settingsNode
	isArray bool
	scalar  json.RawMessage
}

var jsonNull = json.RawMessage("null")

// parseSettingsNode parses one JSON value, rejecting trailing data.
func parseSettingsNode(raw []byte) (*settingsNode, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty JSON value")
	}
	switch trimmed[0] {
	case '{':
		var members map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &members); err != nil {
			return nil, err
		}
		node := &settingsNode{object: make(map[string]*settingsNode, len(members))}
		for key, member := range members {
			child, err := parseSettingsNode(member)
			if err != nil {
				return nil, err
			}
			node.object[key] = child
		}
		return node, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		node := &settingsNode{isArray: true, array: make([]*settingsNode, 0, len(items))}
		for _, item := range items {
			child, err := parseSettingsNode(item)
			if err != nil {
				return nil, err
			}
			node.array = append(node.array, child)
		}
		return node, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, err
	}
	return &settingsNode{scalar: compact.Bytes()}, nil
}

// parseSettingsObject parses a settings document, which must be a JSON object.
func parseSettingsObject(raw []byte) (map[string]*settingsNode, error) {
	node, err := parseSettingsNode(raw)
	if err != nil {
		return nil, err
	}
	if node.object == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	return node.object, nil
}

func stringNode(value string) (*settingsNode, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &settingsNode{scalar: raw}, nil
}

func stringsNode(values []string) (*settingsNode, error) {
	node := &settingsNode{isArray: true, array: make([]*settingsNode, 0, len(values))}
	for _, value := range values {
		item, err := stringNode(value)
		if err != nil {
			return nil, err
		}
		node.array = append(node.array, item)
	}
	return node, nil
}

func boolNode(value bool) *settingsNode {
	return &settingsNode{scalar: json.RawMessage(strconv.FormatBool(value))}
}

// numberNode wraps an already formatted, finite JSON number.
func numberNode(literal string) *settingsNode {
	return &settingsNode{scalar: json.RawMessage(literal)}
}

func (n *settingsNode) isNull() bool {
	return n.object == nil && !n.isArray && bytes.Equal(n.scalar, jsonNull)
}

// isLeaf reports whether attribution treats the node as one value: anything
// but a non-empty object. Arrays are leaves because a higher layer replaces
// them whole.
func (n *settingsNode) isLeaf() bool {
	return n.object == nil || len(n.object) == 0
}

func (n *settingsNode) clone() *settingsNode {
	switch {
	case n.object != nil:
		out := &settingsNode{object: make(map[string]*settingsNode, len(n.object))}
		for key, child := range n.object {
			out.object[key] = child.clone()
		}
		return out
	case n.isArray:
		out := &settingsNode{isArray: true, array: make([]*settingsNode, len(n.array))}
		for i, item := range n.array {
			out.array[i] = item.clone()
		}
		return out
	}
	return &settingsNode{scalar: slices.Clone(n.scalar)}
}

// MarshalJSON writes the node with object keys sorted, which makes the
// encoding canonical for equality checks.
func (n *settingsNode) MarshalJSON() ([]byte, error) {
	switch {
	case n.object != nil:
		return json.Marshal(n.object)
	case n.isArray:
		if n.array == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(n.array)
	}
	return n.scalar, nil
}

// equalSettings compares two values by canonical encoding: object member order
// is ignored, number literals compare as written.
func equalSettings(left, right *settingsNode) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

// mergeMembers applies overlay onto base in place. Objects merge member by
// member, null deletes, and any other value replaces. When entriesReplace is
// set, a named providers entry replaces the lower entry whole, so a settings
// file never inherits another file's credential by reusing a route name.
func mergeMembers(base, overlay map[string]*settingsNode, path []string, entriesReplace bool) {
	for key, value := range overlay {
		memberPath := append(slices.Clip(path), key)
		switch {
		case value.isNull():
			delete(base, key)
		case value.object != nil:
			target := &settingsNode{object: map[string]*settingsNode{}}
			if existing := base[key]; existing != nil && existing.object != nil && !(entriesReplace && isProviderEntry(memberPath)) {
				target = existing
			}
			mergeMembers(target.object, value.object, memberPath, entriesReplace)
			base[key] = target
		default:
			base[key] = value.clone()
		}
	}
}

func isProviderEntry(path []string) bool {
	return len(path) == 2 && path[0] == "providers"
}

// memberAt finds the value at path. exact reports a value at path itself;
// shadowing reports a non-object value at a proper prefix, which replaces or
// unsets whatever path held below it.
func memberAt(members map[string]*settingsNode, path []string) (value *settingsNode, exact, shadowing bool) {
	current := members
	for depth, key := range path {
		child, ok := current[key]
		if !ok {
			return nil, false, false
		}
		if depth == len(path)-1 {
			return child, true, false
		}
		if child.object == nil {
			return child, false, true
		}
		current = child.object
	}
	return nil, false, false
}

// setMember stores value at path, creating or replacing intermediate objects.
func setMember(members map[string]*settingsNode, path []string, value *settingsNode) {
	current := members
	for _, key := range path[:len(path)-1] {
		child := current[key]
		if child == nil || child.object == nil {
			child = &settingsNode{object: map[string]*settingsNode{}}
			current[key] = child
		}
		current = child.object
	}
	current[path[len(path)-1]] = value
}

// deleteMember removes the value at path when it exists.
func deleteMember(members map[string]*settingsNode, path []string) {
	parent := members
	for _, key := range path[:len(path)-1] {
		child := parent[key]
		if child == nil || child.object == nil {
			return
		}
		parent = child.object
	}
	delete(parent, path[len(path)-1])
}

// visitLeaves calls visit for every leaf under members in sorted key order.
func visitLeaves(members map[string]*settingsNode, path []string, visit func(path []string, value *settingsNode)) {
	for _, key := range slices.Sorted(maps.Keys(members)) {
		memberPath := append(slices.Clip(path), key)
		child := members[key]
		if child.isLeaf() {
			visit(memberPath, child)
			continue
		}
		visitLeaves(child.object, memberPath, visit)
	}
}
