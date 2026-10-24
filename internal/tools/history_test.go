package tools

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/permission"
)

func TestSearchHistorySchemaAndReadPermission(t *testing.T) {
	calls := 0
	tool, err := NewTool(SearchHistoryTool, t.TempDir(), ToolsOptions{HistorySearch: func(_ context.Context, query, cursor string, limit int) (json.RawMessage, error) {
		calls++
		if query != "before reset" || cursor != "next" || limit != 3 {
			t.Fatal(query, cursor, limit)
		}
		return json.RawMessage(`{"matches":[],"nextCursor":"later"}`), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaParity(t, string(SearchHistoryTool), tool.Parameters, `{"type":"object","properties":{"query":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["query"]}`)
	result, err := tool.Execute(t.Context(), "search", json.RawMessage(`{"query":"before reset","cursor":"next","limit":3}`), nil)
	if err != nil || calls != 1 || len(result.Content) != 1 {
		t.Fatal(result, err, calls)
	}
	if permission.ClassifyTool(string(SearchHistoryTool)) != permission.TierRead {
		t.Fatal("archive search isn't read-tier")
	}
}
