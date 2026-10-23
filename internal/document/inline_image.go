package document

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"os"
)

// Inline images travel base64-encoded inside the request. Anthropic rejects
// an image whose encoded form exceeds 5 MB or whose side exceeds 8000 px;
// those are the strictest limits among the supported providers, so an image
// within them is accepted everywhere.
const (
	maxInlineImageEncodedBytes = 5 << 20
	// MaxInlineImageBytes is the largest image file whose base64 form fits
	// the encoded limit.
	MaxInlineImageBytes   = maxInlineImageEncodedBytes / 4 * 3
	MaxInlineImageSidePx  = 8000
	inlineImageSniffBytes = 512
)

// InlineImage is an image file ready to send as an image content block.
type InlineImage struct {
	MediaType string
	// Base64 is the standard-encoding payload.
	Base64        string
	SizeBytes     int
	Width, Height int
}

// ReadInlineImage reads an open PNG, JPEG, GIF or WebP file and checks it
// against the inline image limits before encoding it. The caller owns file
// and decides how it was opened (a contained os.Root path or a user path).
func ReadInlineImage(file *os.File) (InlineImage, error) {
	info, err := file.Stat()
	if err != nil {
		return InlineImage{}, err
	}
	if !info.Mode().IsRegular() {
		return InlineImage{}, fmt.Errorf("%s is not a regular file", info.Name())
	}
	if info.Size() > MaxInlineImageBytes {
		return InlineImage{}, fmt.Errorf("%s is %d bytes; inline images are limited to %d bytes (5 MB once base64-encoded)", info.Name(), info.Size(), MaxInlineImageBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxInlineImageBytes+1))
	if err != nil {
		return InlineImage{}, err
	}
	if len(raw) > MaxInlineImageBytes {
		return InlineImage{}, fmt.Errorf("%s grew past %d bytes while it was read", info.Name(), MaxInlineImageBytes)
	}
	mediaType := detectMediaType(raw[:min(len(raw), inlineImageSniffBytes)])
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return InlineImage{}, fmt.Errorf("%s is not a PNG, JPEG, GIF or WebP image (detected %q)", info.Name(), mediaType)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || imageFormatMediaType(format) != mediaType {
		return InlineImage{}, fmt.Errorf("%s is a corrupt %s image", info.Name(), mediaType)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxInlineImageSidePx || config.Height > MaxInlineImageSidePx {
		return InlineImage{}, fmt.Errorf("%s is %dx%d px; inline images are limited to %d px per side", info.Name(), config.Width, config.Height, MaxInlineImageSidePx)
	}
	return InlineImage{MediaType: mediaType, Base64: base64.StdEncoding.EncodeToString(raw), SizeBytes: len(raw), Width: config.Width, Height: config.Height}, nil
}

// ReadInlineImageFile opens path and reads it with [ReadInlineImage].
func ReadInlineImageFile(path string) (InlineImage, error) {
	file, err := os.Open(path)
	if err != nil {
		return InlineImage{}, err
	}
	defer file.Close()
	return ReadInlineImage(file)
}
