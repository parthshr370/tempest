package openai

import (
	"go.harness.dev/harness/internal/provider/conformance"
	"testing"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Factory{Dialect: "openai", Stream: StreamSimple})
}
