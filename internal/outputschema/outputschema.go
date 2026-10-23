// Package outputschema compiles a caller-supplied JSON Schema for a run's final
// answer and validates answers against it. A delegated task's typed yield and
// the root run's -output-schema share this one validator, so both accept and
// reject exactly the same documents.
package outputschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// MaxSchemaBytes bounds a schema document; MaxAnswerBytes bounds the answer
// text validated against it.
const (
	MaxSchemaBytes = 16 << 10
	MaxAnswerBytes = 64 << 10
)

// Schema is a compiled output schema together with its compact source, which
// is what providers receive for native structured output.
type Schema struct {
	raw      json.RawMessage
	compiled *jsonschema.Schema
}

// Compile parses and compiles raw. An empty raw means no schema and returns
// nil, nil. Only local references are allowed: the compiler never loads a
// document from a file or the network.
func Compile(raw json.RawMessage) (*Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("output schema exceeds %d KiB", MaxSchemaBytes>>10)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return nil, errors.New("output schema must be a JSON object")
	}
	if err := localReferences(raw); err != nil {
		return nil, err
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	// A loader with no schemes refuses every external resource.
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err = compiler.AddResource("mem://output", schemaDocument); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile("mem://output")
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return &Schema{raw: compact.Bytes(), compiled: compiled}, nil
}

// Instruction tells the model the final-answer contract for a schema given
// as compact JSON. Providers without native structured output rely on it.
func Instruction(compactSchema json.RawMessage) string {
	return fmt.Sprintf("Your final answer must be exactly one JSON value, at most %d KiB, without Markdown fences or trailing prose, satisfying this JSON Schema:\n%s", MaxAnswerBytes>>10, compactSchema)
}

// Instruction returns [Instruction] for this schema.
func (s *Schema) Instruction() string { return Instruction(s.raw) }

// Raw returns the compact schema document.
func (s *Schema) Raw() json.RawMessage { return bytes.Clone(s.raw) }

// Validate parses text as exactly one JSON value, checks it against the
// schema and returns its compact form.
func (s *Schema) Validate(text string) (json.RawMessage, error) {
	if len(text) > MaxAnswerBytes {
		return nil, fmt.Errorf("answer exceeds %d KiB", MaxAnswerBytes>>10)
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return nil, fmt.Errorf("answer is not one JSON value: %w", err)
	}
	if err = s.compiled.Validate(value); err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, []byte(strings.TrimSpace(text))); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

// localReferences rejects $ref, $dynamicRef, $recursiveRef and $id values
// that point outside the document. It walks only schema-valued keywords, so
// data that merely looks like a reference ("const": {"$ref": ...}) is fine.
func localReferences(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("empty schema node")
	}
	switch raw[0] {
	case '{':
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(raw, &properties); err != nil {
			return err
		}
		for key, value := range properties {
			switch key {
			case "$ref", "$dynamicRef", "$recursiveRef", "$id":
				var reference string
				if err := json.Unmarshal(value, &reference); err != nil {
					return err
				}
				if !(key == "$id" && reference == "") && !strings.HasPrefix(reference, "#") {
					return errors.New("only local schema references and identifiers are allowed")
				}
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
				var schemas map[string]json.RawMessage
				if err := json.Unmarshal(value, &schemas); err != nil {
					return err
				}
				for _, subschema := range schemas {
					if err := localReferences(subschema); err != nil {
						return err
					}
				}
			case "additionalProperties", "unevaluatedProperties", "propertyNames", "contains", "items", "additionalItems", "not", "if", "then", "else", "unevaluatedItems", "contentSchema", "allOf", "anyOf", "oneOf", "prefixItems":
				if err := localReferences(value); err != nil {
					return err
				}
			}
		}
	case '[':
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return err
		}
		for _, element := range elements {
			if err := localReferences(element); err != nil {
				return err
			}
		}
	}
	return nil
}
