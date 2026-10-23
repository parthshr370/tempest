package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
)

// errNoImageInput is view_image's refusal when the session's model has no
// image input: the image would be stripped before the provider sees it.
var errNoImageInput = errors.New("view_image is unavailable: the current model does not accept image input, so it cannot see the image")

// viewImageDetails is the client-facing metadata of a shown image; the bytes
// travel only in the image block.
type viewImageDetails struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bytes     int    `json:"bytes"`
}

// newViewImageTool builds view_image: it loads a PNG, JPEG, GIF or WebP file
// inside the working directory and returns it as an image block, so a vision
// model sees the pixels instead of reading binary as text.
func newViewImageTool(cwd string, options ToolsOptions) agent.AgentTool {
	getRoot := rootOnce(cwd)
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(ViewImageTool),
			Description: fmt.Sprintf("Look at an image file in the workspace (PNG, JPEG, GIF or WebP, up to %d bytes and %d px per side). The image is returned to you as an image you can see. Use it for screenshots, diagrams and rendered output; use read for text files.", document.MaxInlineImageBytes, document.MaxInlineImageSidePx),
			Parameters: schema.Object(schema.JSON{
				"path": schema.JSON{"type": "string", "description": "image file path, relative to the working directory or absolute inside it"},
			}, "path"),
		},
		Label: string(ViewImageTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Path string `json:"path"`
			}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if strings.TrimSpace(input.Path) == "" {
				return agent.AgentToolResult{}, errors.New("view_image requires a path")
			}
			if !options.ImageInput {
				return agent.AgentToolResult{}, errNoImageInput
			}
			if err := ctx.Err(); err != nil {
				return agent.AgentToolResult{}, err
			}
			name, err := resolveRootPath(cwd, input.Path)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			root, err := getRoot()
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			file, err := root.Open(name)
			if err != nil {
				return agent.AgentToolResult{}, wrapOutsideRoot(name, err)
			}
			defer file.Close()
			img, err := document.ReadInlineImage(file)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			details := viewImageDetails{Path: name, MediaType: img.MediaType, Width: img.Width, Height: img.Height, Bytes: img.SizeBytes}
			result := textResult(fmt.Sprintf("Image %s (%s, %dx%d px):", name, img.MediaType, img.Width, img.Height), details)
			result.Content = append(result.Content, types.NewImage(img.Base64, img.MediaType))
			return result, nil
		},
	}
}
