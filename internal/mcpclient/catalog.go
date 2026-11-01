package mcpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/refs"
)

// ResourceScheme prefixes the read tool's MCP resource URLs:
// mcp://<server>/<resource uri>, and mcp://<server>/ for its listing.
const ResourceScheme = "mcp://"

// catalog is one consistent snapshot of what a server offers. A single
// refresh replaces all of it, after startup and on any list_changed
// notification.
type catalog struct {
	tools     []agent.AgentTool
	resources []resource
	templates []resourceTemplate
}

type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

type resourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// discover reads the complete catalog for the capabilities the server
// advertised. Any failure keeps the previous catalog and marks the server
// unavailable, so tools never come from a different snapshot than resources.
func (s *server) discover(ctx context.Context, client *mcpwire.Client) (catalog, error) {
	if client == nil {
		return catalog{}, errors.New("connection unavailable")
	}
	caps := client.Capabilities()
	var next catalog
	var err error
	if caps.Tools {
		if next.tools, err = s.discoverTools(ctx, client); err != nil {
			return catalog{}, err
		}
	}
	if caps.Resources {
		err = paginate(ctx, client, "resources/list", "resources", func(raw json.RawMessage) error {
			var entry resource
			if json.Unmarshal(raw, &entry) != nil || entry.URI == "" || entry.Name == "" {
				return errors.New("mcp_protocol: resource needs uri and name")
			}
			next.resources = append(next.resources, entry)
			return nil
		})
		if err != nil {
			return catalog{}, err
		}
		err = paginate(ctx, client, "resources/templates/list", "resourceTemplates", func(raw json.RawMessage) error {
			var entry resourceTemplate
			if json.Unmarshal(raw, &entry) != nil || entry.URITemplate == "" || entry.Name == "" {
				return errors.New("mcp_protocol: resource template needs uriTemplate and name")
			}
			next.templates = append(next.templates, entry)
			return nil
		})
		if err != nil {
			return catalog{}, err
		}
	}
	return next, nil
}

// paginate walks a cursor-paged list method, bounding pages, entries and
// definition bytes, and hands each raw entry of field to item.
func paginate(ctx context.Context, client *mcpwire.Client, method, field string, item func(json.RawMessage) error) error {
	cursor := ""
	seenCursors := map[string]bool{}
	entries, totalBytes := 0, 0
	for range maxCatalogPages {
		params := struct {
			Cursor string `json:"cursor,omitempty"`
		}{cursor}
		raw, err := client.Call(ctx, method, params)
		if err != nil {
			return err
		}
		var page map[string]json.RawMessage
		var items []json.RawMessage
		if json.Unmarshal(raw, &page) != nil || json.Unmarshal(page[field], &items) != nil || items == nil {
			return fmt.Errorf("mcp_protocol: invalid %s result", method)
		}
		for _, definition := range items {
			entries++
			totalBytes += len(definition)
			if totalBytes > maxDefinitions || entries > maxCatalogEntries {
				return fmt.Errorf("mcp_catalog_limit: too many %s entries", method)
			}
			if err = item(definition); err != nil {
				return err
			}
		}
		cursor = ""
		if next, ok := page["nextCursor"]; ok && string(next) != "null" && json.Unmarshal(next, &cursor) != nil {
			return fmt.Errorf("mcp_protocol: invalid %s cursor", method)
		}
		if cursor == "" {
			return nil
		}
		if seenCursors[cursor] {
			return fmt.Errorf("mcp_protocol: repeated %s cursor", method)
		}
		seenCursors[cursor] = true
	}
	return fmt.Errorf("mcp_catalog_limit: %s exceeds %d pages", method, maxCatalogPages)
}

// current waits for a refresh in flight and returns the server's
// connection and catalog, or why it has none.
func (s *server) current(ctx context.Context) (*mcpwire.Client, catalog, error) {
	s.mu.Lock()
	done, refreshing := s.refreshDone, s.refreshing
	s.mu.Unlock()
	if refreshing {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, catalog{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil || s.unavailable != nil || !s.client.Alive() {
		cause := "not connected"
		if s.unavailable != nil {
			cause = s.unavailable.Error()
		}
		return nil, catalog{}, fmt.Errorf("mcp_disconnected: server %s unavailable (%s); prepare a new run to reconnect", s.config.Name, cause)
	}
	return s.client, s.catalog, nil
}

func (m *Manager) server(name string) (*server, error) {
	names := make([]string, 0, len(m.servers))
	for _, peer := range m.servers {
		if peer.config.Name == name {
			return peer, nil
		}
		names = append(names, peer.config.Name)
	}
	if len(names) == 0 {
		return nil, errors.New("no MCP servers are configured for this session")
	}
	return nil, fmt.Errorf("unknown MCP server %q; configured: %s", name, strings.Join(names, ", "))
}

// ReadResource serves the read tool's mcp:// URLs. mcp:// lists the
// servers, mcp://<server>/ lists one server's resources and templates, and
// mcp://<server>/<uri> reads a resource. Text comes back as Data; a
// supported image blob comes back as an image block; any other blob is
// saved to the session's MCP output directory and referenced in Data.
func (m *Manager) ReadResource(ctx context.Context, raw string) (refs.Resource, []types.ContentBlock, error) {
	rest, ok := strings.CutPrefix(raw, ResourceScheme)
	if !ok {
		return refs.Resource{}, nil, fmt.Errorf("%s: not an mcp:// URL", raw)
	}
	name, uri, _ := strings.Cut(rest, "/")
	if name == "" {
		return refs.Resource{URI: ResourceScheme, Data: []byte(m.renderServers(ctx)), Rendered: true}, nil, nil
	}
	peer, err := m.server(name)
	if err != nil {
		return refs.Resource{}, nil, err
	}
	client, snapshot, err := peer.current(ctx)
	if err != nil {
		return refs.Resource{}, nil, err
	}
	if !client.Capabilities().Resources {
		return refs.Resource{}, nil, fmt.Errorf("MCP server %s offers no resources", name)
	}
	if uri == "" {
		return refs.Resource{URI: ResourceScheme + name + "/", Data: []byte(renderListing(name, snapshot)), Rendered: true}, nil, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, peer.callTimeout)
	defer cancel()
	result, err := peer.call(callCtx, client, "resources/read", struct {
		URI string `json:"uri"`
	}{uri})
	if err != nil {
		if retirable(err) {
			peer.retire(client, err)
		}
		return refs.Resource{}, nil, fmt.Errorf("MCP server %s resources/read %s: %w", name, uri, err)
	}
	data, images, multiple, err := mapContents(m.spillDir, result)
	if err != nil {
		if strings.Contains(err.Error(), "mcp_protocol") {
			peer.retire(client, err)
		}
		return refs.Resource{}, nil, fmt.Errorf("MCP server %s resource %s: %w", name, uri, err)
	}
	return refs.Resource{URI: ResourceScheme + name + "/" + uri, Data: []byte(data), Rendered: multiple}, images, nil
}

// mapContents renders a resources/read result. One text content is returned
// unchanged so line ranges and JSON queries apply to it; several contents
// are each introduced by their URI.
func mapContents(spillDir string, raw json.RawMessage) (string, []types.ContentBlock, bool, error) {
	if len(raw) > mcpwire.MaxResponse || !utf8.Valid(raw) {
		return "", nil, false, errors.New("mcp_protocol: malformed resources/read result")
	}
	var result struct {
		Contents []struct {
			URI      string  `json:"uri"`
			MimeType string  `json:"mimeType"`
			Text     *string `json:"text"`
			Blob     *string `json:"blob"`
		} `json:"contents"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Contents == nil {
		return "", nil, false, errors.New("mcp_protocol: malformed resources/read result")
	}
	spill := newSpill(spillDir, raw)
	multiple := len(result.Contents) != 1
	var text strings.Builder
	var images []types.ContentBlock
	for _, content := range result.Contents {
		if content.URI == "" || (content.Text == nil) == (content.Blob == nil) {
			return "", nil, false, errors.New("mcp_protocol: resource content needs a uri and exactly one of text or blob")
		}
		if multiple {
			fmt.Fprintf(&text, "--- %s (%s)\n", content.URI, mimeOrUnknown(content.MimeType))
		}
		if content.Text != nil {
			text.WriteString(*content.Text)
			if multiple && !strings.HasSuffix(*content.Text, "\n") {
				text.WriteByte('\n')
			}
			continue
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(*content.Blob)
		if err != nil {
			return "", nil, false, errors.New("mcp_protocol: invalid base64 blob")
		}
		if inlineImage(content.MimeType, decoded) {
			images = append(images, types.NewImage(*content.Blob, content.MimeType))
			fmt.Fprintf(&text, "Image %s (%s, %d bytes) follows.\n", content.URI, content.MimeType, len(decoded))
			continue
		}
		path, err := spill.binaryBytes(decoded)
		if err != nil {
			return "", nil, false, err
		}
		fmt.Fprintf(&text, "Binary resource %s (%s, %d bytes) saved locally: %s\n", content.URI, mimeOrUnknown(content.MimeType), len(decoded), path)
		multiple = true
	}
	return text.String(), images, multiple, nil
}

// inlineImage reports an image the model can take inline: a supported type
// whose bytes match it, within the inline content budget.
func inlineImage(mimeType string, data []byte) bool {
	supported := mimeType == "image/png" || mimeType == "image/jpeg" || mimeType == "image/gif" || mimeType == "image/webp"
	return supported && http.DetectContentType(data) == mimeType && base64.StdEncoding.EncodedLen(len(data)) <= maxInlineContent-spillReferenceReserve
}

func mimeOrUnknown(mimeType string) string {
	if mimeType == "" {
		return "unknown type"
	}
	return mimeType
}

func renderListing(name string, snapshot catalog) string {
	var out strings.Builder
	fmt.Fprintf(&out, "MCP server %s: %d resources, %d resource templates. Read one with read mcp://%s/<uri>.\n", name, len(snapshot.resources), len(snapshot.templates), name)
	resources := append([]resource(nil), snapshot.resources...)
	sort.SliceStable(resources, func(i, j int) bool { return resources[i].URI < resources[j].URI })
	if len(resources) > 0 {
		out.WriteString("\nResources:\n")
	}
	for _, entry := range resources {
		describe(&out, entry.URI, entry.Name, entry.Title, entry.MimeType, entry.Description)
	}
	if len(snapshot.templates) > 0 {
		out.WriteString("\nResource templates (fill in each {variable}, then read mcp://" + name + "/<uri>):\n")
	}
	for _, entry := range snapshot.templates {
		describe(&out, entry.URITemplate, entry.Name, entry.Title, entry.MimeType, entry.Description)
	}
	return out.String()
}

func describe(out *strings.Builder, uri, name, title, mimeType, description string) {
	label := name
	if title != "" && title != name {
		label += " (" + title + ")"
	}
	fmt.Fprintf(out, "- %s  %s", uri, label)
	if mimeType != "" {
		fmt.Fprintf(out, " [%s]", mimeType)
	}
	if description = strings.Join(strings.Fields(description), " "); description != "" {
		out.WriteString(": " + utf8Head(description, 512))
	}
	out.WriteByte('\n')
}

func (m *Manager) renderServers(ctx context.Context) string {
	if len(m.servers) == 0 {
		return "No MCP servers are configured for this session.\n"
	}
	var out strings.Builder
	out.WriteString("MCP servers. List one with read mcp://<server>/.\n")
	for _, peer := range m.servers {
		client, snapshot, err := peer.current(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(&out, "- %s: %v\n", peer.config.Name, err)
		case !client.Capabilities().Resources:
			fmt.Fprintf(&out, "- %s: no resources\n", peer.config.Name)
		default:
			fmt.Fprintf(&out, "- %s: %d resources, %d resource templates\n", peer.config.Name, len(snapshot.resources), len(snapshot.templates))
		}
	}
	return out.String()
}
