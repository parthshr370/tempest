package anthropic

import (
	"go.harness.dev/harness/internal/provider/conformance"
	"testing"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Factory{Dialect: "anthropic", Stream: StreamSimple})
}
