package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The read tool's JSON query (file.json?q=<expr>) is a small jq subset:
//
//	.            the whole document
//	.a.b         object fields; ."key" and .["key"] quote any key
//	.[0] .[-1]   array elements, negative from the end
//	.[]          every element of an array, or every value of an object
//	?            after any step: skip values of the wrong type instead of failing
//	keys         sorted object keys, or array indices
//	length       string, array or object size; 0 for null
//	a | b        feed every result of a into b
//
// Every result prints as indented JSON, one after another; object keys
// print sorted.

// jsonStep maps one value to zero or more values.
type jsonStep func(value any) ([]any, error)

// runJSONQuery parses data as one JSON value and applies expr to it.
func runJSONQuery(data []byte, expr string) (string, error) {
	steps, err := parseJSONQuery(expr)
	if err != nil {
		return "", fmt.Errorf("json query %q: %w", expr, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return "", fmt.Errorf("json query needs a JSON file: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("json query needs a single JSON value; the file has more (JSON Lines is not supported)")
	}
	values := []any{document}
	for _, step := range steps {
		next := []any{}
		for _, value := range values {
			out, err := step(value)
			if err != nil {
				return "", fmt.Errorf("json query %q: %w", expr, err)
			}
			next = append(next, out...)
		}
		values = next
	}
	if len(values) == 0 {
		return "(query produced no results)", nil
	}
	parts := make([]string, len(values))
	for i, value := range values {
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", err
		}
		parts[i] = string(encoded)
	}
	return strings.Join(parts, "\n"), nil
}

// parseJSONQuery compiles expr into a flat list of steps; a pipe simply
// continues the list, since every step already maps over all values.
func parseJSONQuery(expr string) ([]jsonStep, error) {
	var steps []jsonStep
	for _, term := range splitPipes(expr) {
		term = strings.TrimSpace(term)
		switch term {
		case "":
			return nil, errors.New("empty filter")
		case "keys":
			steps = append(steps, jsonKeys)
			continue
		case "length":
			steps = append(steps, jsonLength)
			continue
		}
		pathSteps, err := parseJSONPath(term)
		if err != nil {
			return nil, err
		}
		steps = append(steps, pathSteps...)
	}
	return steps, nil
}

// splitPipes splits on | outside quoted keys.
func splitPipes(expr string) []string {
	var parts []string
	start, inString := 0, false
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '\\':
			if inString {
				i++
			}
		case '"':
			inString = !inString
		case '|':
			if !inString {
				parts = append(parts, expr[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, expr[start:])
}

func parseJSONPath(term string) ([]jsonStep, error) {
	if !strings.HasPrefix(term, ".") {
		return nil, fmt.Errorf("unsupported filter %q; use .field, .[n], .[], keys, length and |", term)
	}
	var steps []jsonStep
	i := 0
	for i < len(term) {
		switch {
		case term[i] == '.' && (i+1 == len(term) || term[i+1] == '['):
			// "." alone is identity; ".[" continues with a bracket step.
			i++
		case term[i] == '.':
			i++
			var key string
			if term[i] == '"' {
				quoted, n, err := readQuoted(term[i:])
				if err != nil {
					return nil, err
				}
				key, i = quoted, i+n
			} else {
				start := i
				for i < len(term) && isIdentByte(term[i]) {
					i++
				}
				if start == i {
					return nil, fmt.Errorf("unexpected %q after '.'", term[i:])
				}
				key = term[start:i]
			}
			optional := consumeOptional(term, &i)
			steps = append(steps, jsonField(key, optional))
		case term[i] == '[':
			closing := strings.IndexByte(term[i:], ']')
			if strings.HasPrefix(term[i+1:], `"`) {
				quoted, n, err := readQuoted(term[i+1:])
				if err != nil {
					return nil, err
				}
				if i+1+n >= len(term) || term[i+1+n] != ']' {
					return nil, errors.New("missing ] after quoted key")
				}
				i += n + 2
				steps = append(steps, jsonField(quoted, consumeOptional(term, &i)))
				continue
			}
			if closing < 0 {
				return nil, errors.New("missing ]")
			}
			inner := strings.TrimSpace(term[i+1 : i+closing])
			i += closing + 1
			optional := consumeOptional(term, &i)
			if inner == "" {
				steps = append(steps, jsonIterate(optional))
				continue
			}
			index, err := strconv.Atoi(inner)
			if err != nil {
				return nil, fmt.Errorf("unsupported index [%s]; use a whole number or a quoted key", inner)
			}
			steps = append(steps, jsonIndex(index, optional))
		default:
			return nil, fmt.Errorf("unexpected %q", term[i:])
		}
	}
	return steps, nil
}

func consumeOptional(term string, i *int) bool {
	if *i < len(term) && term[*i] == '?' {
		*i++
		return true
	}
	return false
}

// readQuoted decodes a JSON string literal at the start of s and returns
// it with the number of bytes consumed.
func readQuoted(s string) (string, int, error) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var key string
			if err := json.Unmarshal([]byte(s[:i+1]), &key); err != nil {
				return "", 0, fmt.Errorf("bad quoted key %s", s[:i+1])
			}
			return key, i + 1, nil
		}
	}
	return "", 0, errors.New("unterminated quoted key")
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= utf8.RuneSelf
}

func jsonField(key string, optional bool) jsonStep {
	return func(value any) ([]any, error) {
		switch typed := value.(type) {
		case map[string]any:
			return []any{typed[key]}, nil
		case nil:
			return []any{nil}, nil
		}
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot index %s with %q", jsonType(value), key)
	}
}

func jsonIndex(index int, optional bool) jsonStep {
	return func(value any) ([]any, error) {
		switch typed := value.(type) {
		case []any:
			at := index
			if at < 0 {
				at += len(typed)
			}
			if at < 0 || at >= len(typed) {
				return []any{nil}, nil
			}
			return []any{typed[at]}, nil
		case nil:
			return []any{nil}, nil
		}
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot index %s with a number", jsonType(value))
	}
}

func jsonIterate(optional bool) jsonStep {
	return func(value any) ([]any, error) {
		switch typed := value.(type) {
		case []any:
			return typed, nil
		case map[string]any:
			keys := sortedKeys(typed)
			out := make([]any, len(keys))
			for i, key := range keys {
				out[i] = typed[key]
			}
			return out, nil
		}
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot iterate over %s", jsonType(value))
	}
}

func jsonKeys(value any) ([]any, error) {
	switch typed := value.(type) {
	case map[string]any:
		keys := sortedKeys(typed)
		out := make([]any, len(keys))
		for i, key := range keys {
			out[i] = key
		}
		return []any{out}, nil
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = i
		}
		return []any{out}, nil
	}
	return nil, fmt.Errorf("%s has no keys", jsonType(value))
}

func jsonLength(value any) ([]any, error) {
	switch typed := value.(type) {
	case nil:
		return []any{0}, nil
	case string:
		return []any{utf8.RuneCountInString(typed)}, nil
	case []any:
		return []any{len(typed)}, nil
	case map[string]any:
		return []any{len(typed)}, nil
	case json.Number:
		if number, err := typed.Float64(); err == nil {
			if number < 0 {
				number = -number
			}
			return []any{number}, nil
		}
	}
	return nil, fmt.Errorf("%s has no length", jsonType(value))
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", value)
}
