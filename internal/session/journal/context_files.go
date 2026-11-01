package journal

// KindContextFile records a nested context file shown after a tool result.
const KindContextFile EntryType = "context_file"

// ContextFileRecord marks a directory whose context file this session already
// surfaced, so a resumed session neither repeats it nor overspends the
// context_files byte cap. Dir and Path are workspace-relative slash paths.
type ContextFileRecord struct {
	Meta  `json:"-"`
	Dir   string `json:"dir"`
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

func (ContextFileRecord) kind() EntryType            { return KindContextFile }
func (e ContextFileRecord) meta() Meta               { return e.Meta }
func (e ContextFileRecord) withMeta(meta Meta) Entry { e.Meta = meta; return e }
