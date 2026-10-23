package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// maxResultBytes bounds Content plus Details: Value emitted raw and as an
	// escaped JSON string.
	maxResultBytes = 64 << 10
	// maxDecodedLocations bounds decoding before any normalization.
	maxDecodedLocations = 10000
	// maxLocationFiles and maxLocationFileBytes bound unopened files read to
	// convert returned positions.
	maxLocationFiles     = 64
	maxLocationFileBytes = 16 << 20
)

type rangeOut struct {
	Line      int `json:"line"`
	Column    int `json:"column"`
	EndLine   int `json:"end_line"`
	EndColumn int `json:"end_column"`
}

type locationOut struct {
	Path string `json:"path"`
	rangeOut
	TargetRange *rangeOut `json:"target_range,omitempty"`
}

type locationsResult struct {
	Server                   string        `json:"server"`
	Path                     string        `json:"path"`
	Version                  int32         `json:"version"`
	Anchor                   string        `json:"anchor"`
	Locations                []locationOut `json:"locations"`
	Total                    int           `json:"total"`
	Omitted                  int           `json:"omitted"`
	Truncated                bool          `json:"truncated"`
	ExternalLocationsOmitted int           `json:"external_locations_omitted"`
}

// serverLocation is one decoded Location or LocationLink.
type serverLocation struct {
	path   string
	rng    lspRange
	target *lspRange
}

type rawLocation struct {
	URI                  *string   `json:"uri"`
	Range                *lspRange `json:"range"`
	TargetURI            *string   `json:"targetUri"`
	TargetRange          *lspRange `json:"targetRange"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

// encodedCost is the size of Value as raw details plus an escaped text block.
func encodedCost(value []byte) int {
	escaped, _ := json.Marshal(string(value))
	return len(value) + len(escaped)
}

// fitResult returns build(k) for the largest k <= n whose encoded cost fits
// limit, dropping trailing entries rather than cutting JSON.
func fitResult(n, limit int, build func(int) ([]byte, error)) (json.RawMessage, error) {
	value, err := build(n)
	if err != nil {
		return nil, err
	}
	if encodedCost(value) <= limit {
		return value, nil
	}
	low, high := 0, n-1
	var best []byte
	for low <= high {
		mid := (low + high) / 2
		candidate, err := build(mid)
		if err != nil {
			return nil, err
		}
		if encodedCost(candidate) <= limit {
			best, low = candidate, mid+1
		} else {
			high = mid - 1
		}
	}
	if best == nil {
		return nil, newError(CodeOutputLimit, "result doesn't fit the 64 KiB tool result budget")
	}
	return best, nil
}

// decodeArray streams a JSON array, calling each for at most limit elements.
func decodeArray(raw json.RawMessage, limit int, each func(json.RawMessage) error) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return newError(CodeProtocol, "expected a JSON array")
	}
	for count := 0; decoder.More(); count++ {
		if count == limit {
			return newError(CodeOutputLimit, fmt.Sprintf("server returned more than %d locations", limit))
		}
		var element json.RawMessage
		if err := decoder.Decode(&element); err != nil {
			return newError(CodeProtocol, "malformed array element")
		}
		if err := each(element); err != nil {
			return err
		}
	}
	return nil
}

// decodeLocations normalizes null, Location, Location[] and LocationLink[].
// Non-file or uncontained URIs are counted as external.
func decodeLocations(root string, raw json.RawMessage) (locations []serverLocation, total, external int, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, 0, 0, nil
	}
	add := func(element json.RawMessage) error {
		var item rawLocation
		if json.Unmarshal(element, &item) != nil {
			return newError(CodeProtocol, "malformed location")
		}
		total++
		var uri string
		var location serverLocation
		switch {
		case item.TargetURI != nil && item.TargetSelectionRange != nil && item.TargetRange != nil:
			uri, location.rng = *item.TargetURI, *item.TargetSelectionRange
			target := *item.TargetRange
			location.target = &target
		case item.URI != nil && item.Range != nil:
			uri, location.rng = *item.URI, *item.Range
		default:
			return newError(CodeProtocol, "location has neither uri/range nor targetUri/targetRange/targetSelectionRange")
		}
		rel, ok := uriPath(root, uri)
		if !ok {
			external++
			return nil
		}
		location.path = rel
		locations = append(locations, location)
		return nil
	}
	if raw[0] == '[' {
		err = decodeArray(raw, maxDecodedLocations, add)
	} else {
		err = add(raw)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	sort.SliceStable(locations, func(i, j int) bool {
		a, b := locations[i], locations[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.rng.Start != b.rng.Start {
			return a.rng.Start.less(b.rng.Start)
		}
		return a.rng.End.less(b.rng.End)
	})
	deduped := locations[:0]
	for i, location := range locations {
		if i > 0 && location.path == locations[i-1].path && location.rng == locations[i-1].rng {
			continue
		}
		deduped = append(deduped, location)
	}
	return deduped, total, external, nil
}

// locationText returns line indexes for location files: open documents use
// the synchronized text, others a bounded contained read.
type locationText struct {
	s     *server
	files map[string]*lineIndex
	bytes int
}

func (t *locationText) index(ctx context.Context, rel string) (*lineIndex, error) {
	if index, ok := t.files[rel]; ok {
		return index, nil
	}
	if record := t.s.openDocument(rel); record != nil {
		t.files[rel] = record.index
		return record.index, nil
	}
	if len(t.files) >= maxLocationFiles {
		return nil, nil
	}
	doc, err := t.s.m.ws.ReadFile(ctx, rel)
	if ctx.Err() != nil {
		return nil, asError(ctx.Err())
	}
	if err != nil || doc.Path != rel || validateDocument(doc) != nil || t.bytes+len(doc.Text) > maxLocationFileBytes {
		t.files[rel] = nil
		return nil, nil
	}
	t.bytes += len(doc.Text)
	index := newLineIndex(doc.Text)
	t.files[rel] = index
	return index, nil
}

// locationsValue converts server locations to bounded model positions.
// total counts decoded locations before filters, so
// total == len(locations) + omitted + external_locations_omitted.
func (s *server) locationsValue(ctx context.Context, record *openDoc, raw json.RawMessage, maxResults int) (json.RawMessage, error) {
	decoded, total, external, err := decodeLocations(s.m.root, raw)
	if err != nil {
		return nil, err
	}
	texts := &locationText{s: s, files: map[string]*lineIndex{}}
	converted := make([]locationOut, 0, min(len(decoded), maxResults))
	limited := false
	for _, location := range decoded {
		if len(converted) == maxResults {
			limited = true
			break
		}
		index, err := texts.index(ctx, location.path)
		if err != nil {
			return nil, err
		}
		if index == nil {
			continue
		}
		start, end, err := index.modelRange(location.rng)
		if err != nil {
			continue
		}
		out := locationOut{Path: location.path, rangeOut: rangeOut{Line: start.Line, Column: start.Column, EndLine: end.Line, EndColumn: end.Column}}
		if location.target != nil {
			targetStart, targetEnd, err := index.modelRange(*location.target)
			if err != nil {
				continue
			}
			out.TargetRange = &rangeOut{Line: targetStart.Line, Column: targetStart.Column, EndLine: targetEnd.Line, EndColumn: targetEnd.Column}
		}
		converted = append(converted, out)
	}
	return fitResult(len(converted), maxResultBytes, func(n int) ([]byte, error) {
		omitted := total - external - n
		return json.Marshal(locationsResult{
			Server: s.route.name, Path: record.path, Version: record.version, Anchor: record.anchor,
			Locations: converted[:n], Total: total, Omitted: omitted, Truncated: limited || n < len(converted),
			ExternalLocationsOmitted: external,
		})
	})
}
