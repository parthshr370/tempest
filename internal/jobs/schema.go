package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxSchemaBytes = 16 << 10
const maxYieldBytes = 64 << 10

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("%w: output_schema exceeds 16 KiB", ErrInvalidInput)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return nil, fmt.Errorf("%w: output_schema must be an object", ErrInvalidInput)
	}
	if err := localReferences(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err = compiler.AddResource("mem://yield", schemaDocument); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	compiled, err := compiler.Compile("mem://yield")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return compiled, nil
}

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

func validateYield(compiled *jsonschema.Schema, text string) (json.RawMessage, error) {
	if len(text) > maxYieldBytes {
		return nil, errors.New("yield exceeds 64 KiB")
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	if err = compiled.Validate(value); err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, []byte(text)); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
