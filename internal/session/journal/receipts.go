package journal

import (
	"encoding/json"

	types "go.harness.dev/harness/internal/engine/types"
)

// UsageSource reports a receipt already committed by this writer. It includes
// metered-message fallback and compaction facts, not another journal's promises.
func (s *Store) UsageSource(source string) (types.Usage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage, ok := s.usageSources[source]
	return usage, ok
}

// UsageSources copies the writer's committed receipts without replaying the archive.
func (s *Store) UsageSources() map[string]types.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	sources := make(map[string]types.Usage, len(s.usageSources))
	for source, usage := range s.usageSources {
		sources[source] = usage
	}
	return sources
}

func (s *Store) rememberUsage(entry Entry, meta Meta) {
	var source string
	var usage types.Usage
	switch e := entry.(type) {
	case MessageEntry:
		if e.Metered {
			if assistant, ok := asAssistantMessage(e.Message); ok {
				source = "message:" + s.id + ":" + meta.ID
				usage = assistant.Usage
			}
		}
	case UsageRecord:
		source = e.SourceRef
		usage = e.Usage
		if e.MessageID != "" {
			source = "message:" + s.id + ":" + e.MessageID
		}
	case WorkRecord:
		if e.Type == KindCompactionCallFinished {
			var call CompactionCall
			if json.Unmarshal(e.Payload, &call) == nil {
				source = call.SourceRef
				usage = call.Usage
			}
		}
	}
	if source != "" {
		if s.usageSources == nil {
			s.usageSources = map[string]types.Usage{}
		}
		if _, exists := s.usageSources[source]; !exists {
			s.usageSources[source] = usage
		}
	}
}
