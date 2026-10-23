package jobs

import (
	"encoding/json"
	"fmt"

	"go.harness.dev/harness/internal/outputschema"
)

// compileSchema compiles a task's output_schema with the shared validator; a
// malformed schema is the caller's input error.
func compileSchema(raw json.RawMessage) (*outputschema.Schema, error) {
	compiled, err := outputschema.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return compiled, nil
}
