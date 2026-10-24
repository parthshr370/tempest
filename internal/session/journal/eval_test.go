package journal

import (
	"encoding/json"
	"testing"
)

func TestEvalLanguagesSurviveTranscriptCompaction(t *testing.T) {
	loaded := &Loaded{}
	reduce(loaded, []Entry{
		ToolStartedRecord{Tool: "eval", Args: json.RawMessage(`{"language":"py","code":"answer = 42"}`)},
		CompactionEntry{},
		ToolStartedRecord{Tool: "eval", Args: json.RawMessage(`{"language":"js","code":"let answer = 42"}`)},
		ToolStartedRecord{Tool: "eval", Args: json.RawMessage(`{"language":"invalid"}`)},
		ToolStartedRecord{Tool: "bash", Args: json.RawMessage(`{"language":"other"}`)},
	})
	if len(loaded.EvalLanguages) != 2 || !loaded.EvalLanguages["py"] || !loaded.EvalLanguages["js"] {
		t.Fatalf("used languages weren't retained: %+v", loaded.EvalLanguages)
	}
}
