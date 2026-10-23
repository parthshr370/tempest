package main

import (
	"fmt"
	"os"

	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/tempest"
)

// buildOneShotPrompt turns -p, -output-schema and -image into the run's
// prompt. Every input is checked here, before a session is written, so a bad
// schema, an oversized image or a text-only model fails with a clear message
// instead of a provider error mid-run.
func buildOneShotPrompt(opts commandOptions, model ptypes.Model) (tempest.Prompt, error) {
	prompt := tempest.Prompt{Text: opts.Prompt}
	if opts.OutputSchemaPath != "" {
		raw, err := os.ReadFile(opts.OutputSchemaPath)
		if err != nil {
			return tempest.Prompt{}, fmt.Errorf("-output-schema: %w", err)
		}
		if _, err := outputschema.Compile(raw); err != nil {
			return tempest.Prompt{}, fmt.Errorf("-output-schema %s: %w", opts.OutputSchemaPath, err)
		}
		prompt.OutputSchema = raw
	}
	if len(opts.ImagePaths) > 0 && !model.SupportsVision() {
		return tempest.Prompt{}, fmt.Errorf("-image: model %s/%s does not accept image input", model.Provider, model.ID)
	}
	for _, path := range opts.ImagePaths {
		img, err := document.ReadInlineImageFile(path)
		if err != nil {
			return tempest.Prompt{}, fmt.Errorf("-image %s: %w", path, err)
		}
		prompt.Media = append(prompt.Media, ptypes.NewImage(img.Base64, img.MediaType))
	}
	return prompt, nil
}
