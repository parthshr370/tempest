package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go.harness.dev/harness/tempest/durable"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CatalogEntry holds archive metadata without reducing its transcript.
type CatalogEntry struct {
	Header      Header
	Path        string
	UpdatedAt   time.Time
	Initialized bool
	HostManaged bool
	LastRun     *RunSummary
}

// Catalog scans root session headers, excluding task children and artifacts.
func Catalog(ctx context.Context, dir, cwd string) ([]CatalogEntry, error) {
	roots, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []CatalogEntry
	for _, root := range roots {
		if !root.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(dir, root.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, root.Name(), entry.Name())
			header, err := loadHeader(ctx, path)
			if err != nil {
				slog.Warn("skipping unreadable session journal", "path", path, "error", err)
				continue
			}
			if cwd != "" && cwd != header.Cwd {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				slog.Warn("skipping unavailable session journal", "path", path, "error", err)
				continue
			}
			var lastRun *RunSummary
			initialized, managed, err := catalogReadiness(ctx, path, header, &lastRun)
			if err != nil {
				slog.Warn("skipping uninitialized session journal", "path", path, "error", err)
				continue
			}
			result = append(result, CatalogEntry{Header: header, Path: path, UpdatedAt: info.ModTime(), Initialized: initialized, HostManaged: managed, LastRun: lastRun})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result, nil
}

type catalogMetadata struct {
	Initialized     bool        `json:"initialized"`
	HostManaged     bool        `json:"hostManaged"`
	LastRun         *RunSummary `json:"lastRun,omitempty"`
	JournalBytes    int64       `json:"journalBytes"`
	RequestsIndexed bool        `json:"requestsIndexed,omitempty"`
	Requests        []string    `json:"requests,omitempty"`
}

func catalogReadiness(ctx context.Context, path string, header Header, lastRun **RunSummary) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	var metadata catalogMetadata
	raw, err := os.ReadFile(path + ".catalog")
	if err == nil {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return false, false, statErr
		}
		if err = json.Unmarshal(raw, &metadata); err != nil {
			return false, false, err
		}
		*lastRun = metadata.LastRun
		if metadata.JournalBytes == info.Size() {
			return metadata.Initialized, metadata.HostManaged, nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return false, false, err
	}
	// Old archives have no index. Inspect bounded metadata windows, not the
	// transcript or checksums. Opening the session validates its full journal.
	const metadataWindow = 1 << 20
	file, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer file.Close()
	prefix, err := io.ReadAll(io.LimitReader(file, metadataWindow))
	if err != nil {
		return false, false, err
	}
	initialized, managed := header.Version < 4 || metadata.Initialized, metadata.HostManaged
	observe := func(raw []byte) {
		entry, decodeErr := decodeEntry(raw)
		if decodeErr != nil {
			return
		}
		switch record := entry.(type) {
		case RunStartedRecord:
			*lastRun = &RunSummary{RunID: record.RunID, LogicalRunID: record.LogicalRunID, ContinuationOf: record.ContinuationOf, StartedAt: record.Timestamp}
		case RunFinishedRecord:
			if *lastRun == nil || (*lastRun).RunID != record.RunID {
				*lastRun = &RunSummary{RunID: record.RunID}
			}
			(*lastRun).Finished, (*lastRun).StopReason, (*lastRun).Error, (*lastRun).FinishedAt = true, record.StopReason, record.Error, record.Timestamp
		}
	}
	scan := func(contents []byte) {
		for _, line := range bytes.Split(contents, []byte{'\n'}) {
			if header.Version < 4 {
				observe(line)
				continue
			}
			var envelope durable.Envelope
			if json.Unmarshal(line, &envelope) != nil {
				continue
			}
			for _, change := range envelope.Commit.Changes {
				if change.Kind == durable.EntryKind && change.Namespace == "_tempest/journal" {
					observe(change.Value)
				}
				if change.Kind == durable.DocumentKind && change.Namespace == "_tempest/agent" && change.ID == "selection" {
					var choice struct {
						HostManaged bool `json:"hostManaged"`
					}
					if json.Unmarshal(change.Value, &choice) == nil {
						initialized, managed = true, choice.HostManaged
					}
				}
			}
		}
	}
	scan(prefix)
	info, err := file.Stat()
	if err != nil {
		return false, false, err
	}
	if info.Size() > metadataWindow {
		if _, err = file.Seek(max(0, info.Size()-metadataWindow), io.SeekStart); err != nil {
			return false, false, err
		}
		suffix, err := io.ReadAll(io.LimitReader(file, metadataWindow))
		if err != nil {
			return false, false, err
		}
		if boundary := bytes.IndexByte(suffix, '\n'); boundary >= 0 {
			scan(suffix[boundary+1:])
		}
	}
	return initialized, managed, nil
}

// MarkInitialized publishes catalog readiness after the agent choice commits.
func (s *Store) MarkInitialized(managed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.external {
		return nil
	}
	metadata, err := s.readCatalogMetadata()
	if err != nil {
		return err
	}
	metadata.Initialized, metadata.HostManaged = true, managed
	if metadata.LastRun == nil {
		metadata.LastRun = s.catalogLastRun
	}
	return s.writeCatalogMetadata(metadata)
}

func (s *Store) readCatalogMetadata() (catalogMetadata, error) {
	var metadata catalogMetadata
	raw, err := os.ReadFile(s.path + ".catalog")
	if os.IsNotExist(err) {
		return metadata, nil
	}
	if err != nil {
		return metadata, err
	}
	err = json.Unmarshal(raw, &metadata)
	return metadata, err
}

func (s *Store) writeCatalogMetadata(metadata catalogMetadata) error {
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	metadata.JournalBytes = info.Size()
	metadata.Requests = nil
	if s.durable != nil {
		query := durable.Query{SessionID: s.id, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Limit: 1000}
		for {
			page, err := s.durable.Read(context.Background(), query)
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				metadata.Requests = append(metadata.Requests, record.ID)
			}
			if page.NextCursor == "" {
				metadata.RequestsIndexed = true
				break
			}
			query.Cursor = page.NextCursor
		}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".catalog-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_, err = file.Write(raw)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.path+".catalog"); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(s.path))
}

func (s *Store) updateCatalogRun(raw []byte) {
	if s.external {
		return
	}
	entry, err := decodeEntry(raw)
	if err != nil {
		return
	}
	switch entry.(type) {
	case RunStartedRecord, RunFinishedRecord:
	default:
		return
	}
	metadata, err := s.readCatalogMetadata()
	if err == nil {
		switch record := entry.(type) {
		case RunStartedRecord:
			metadata.LastRun = &RunSummary{RunID: record.RunID, LogicalRunID: record.LogicalRunID, ContinuationOf: record.ContinuationOf, StartedAt: record.Timestamp}
		case RunFinishedRecord:
			if metadata.LastRun == nil || metadata.LastRun.RunID != record.RunID {
				metadata.LastRun = &RunSummary{RunID: record.RunID}
			}
			metadata.LastRun.Finished, metadata.LastRun.StopReason, metadata.LastRun.Error, metadata.LastRun.FinishedAt = true, record.StopReason, record.Error, record.Timestamp
		}
		s.catalogLastRun = metadata.LastRun
		err = s.writeCatalogMetadata(metadata)
	}
	if err != nil {
		slog.Warn("session catalog metadata update failed", "path", s.path, "error", err)
	}
}

func (s *Store) updateCatalogSubmissions(commit durable.Commit) {
	if s.external {
		return
	}
	hasSubmission := false
	for _, change := range commit.Changes {
		if change.Kind == durable.SubmissionKind && change.Namespace == "_tempest/submissions" {
			hasSubmission = true
			break
		}
	}
	if !hasSubmission {
		return
	}
	metadata, err := s.readCatalogMetadata()
	if err == nil {
		err = s.writeCatalogMetadata(metadata)
	}
	if err != nil {
		slog.Warn("session request index update failed", "path", s.path, "error", err)
	}
}
