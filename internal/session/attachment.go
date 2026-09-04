package session

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/document"
)

// ResolveLocalAttachments reads local files named on the command line into the
// session blob store and registers each one so the read-only attachment tool
// can serve it. The returned reader is wired into [StackConfig] beside the
// registry. The namespace is a fixed session id: the CLI runs one session per
// process, so every attachment shares one blob namespace and the
// CacheRootBlobReader can resolve the (store, key) reference without a
// session-id handoff. A missing path is a configuration error: the user asked
// for a file that does not exist, and silently dropping it would leave the
// model answering without content it was told it has.
func ResolveLocalAttachments(ctx context.Context, paths []string, cacheRoot string) (*document.AttachmentRegistry, *document.CacheRootBlobReader, error) {
	registry := document.NewAttachmentRegistry()
	reader := document.NewCacheRootBlobReader(cacheRoot)
	if len(paths) == 0 {
		return registry, reader, nil
	}
	namespace := document.SessionNamespace("cli")
	store, err := document.NewLocalBlobStore(filepath.Join(cacheRoot, namespace))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range paths {
		entry, err := resolveOneAttachment(ctx, store, namespace, path)
		if err != nil {
			return nil, nil, err
		}
		registry.Put(entry)
	}
	return registry, reader, nil
}

// resolveOneAttachment stores one local file's bytes under the shared namespace
// and returns its registry entry. The blob key is content-addressed, so the
// raw bytes and any later derived representation stay deduplicated.
func resolveOneAttachment(ctx context.Context, store document.BlobStore, namespace, path string) (document.AttachmentEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return document.AttachmentEntry{}, err
	}
	filename := filepath.Base(path)
	mediaType := mime.TypeByExtension(filepath.Ext(path))
	if mediaType == "" {
		mediaType = detectAttachmentMediaType(data)
	}
	if before, _, found := strings.Cut(mediaType, ";"); found {
		mediaType = before
	}
	ref, err := store.Put(ctx, bytes.NewReader(data), document.UploadMetadata{Filename: filename, MediaType: mediaType})
	if err != nil {
		return document.AttachmentEntry{}, err
	}
	return document.AttachmentEntry{
		ID:           filename,
		Filename:     filename,
		MediaType:    mediaType,
		SizeBytes:    int64(len(data)),
		Blob:         document.BlobRef{Store: namespace, Key: ref.Key},
		TextReadable: isTextMediaType(mediaType),
	}, nil
}

// detectAttachmentMediaType sniffs the bytes for extensionless files so the
// attachment tool can still report something useful.
func detectAttachmentMediaType(data []byte) string {
	if len(data) > 512 {
		data = data[:512]
	}
	if detected := http.DetectContentType(data); detected != "application/octet-stream" {
		return detected
	}
	return "application/octet-stream"
}
