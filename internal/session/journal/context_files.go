package journal

// ContextFileRecord marks a directory whose context file this session already
// surfaced, so a resumed session neither repeats it nor overspends the
// context_files byte cap. Dir and Path are workspace-relative slash paths.
// Withheld marks a file named instead of loaded because the cap was spent.
//
// It is journaled inside the MessageEntry of the tool result that delivered
// the file, never on its own: the record and the delivered text commit
// together or not at all.
type ContextFileRecord struct {
	Dir      string `json:"dir"`
	Path     string `json:"path"`
	Bytes    int    `json:"bytes"`
	Withheld bool   `json:"withheld,omitempty"`
}

// StageContextFiles holds the context files a tool call's result carries
// until the recorder commits that result; callID is the call's canonical
// invocation id, or its provider id outside durable admission.
func (s *Store) StageContextFiles(callID string, files []ContextFileRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stagedContext == nil {
		s.stagedContext = map[string][]ContextFileRecord{}
	}
	s.stagedContext[callID] = append(s.stagedContext[callID], files...)
}

// TakeContextFiles returns and forgets what StageContextFiles holds for
// callID, for the MessageEntry that commits its result.
func (s *Store) TakeContextFiles(callID string) []ContextFileRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := s.stagedContext[callID]
	delete(s.stagedContext, callID)
	return files
}
