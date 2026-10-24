package serve

import (
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestLiveSessionLoadPreservesSymlinkCwd(t *testing.T) {
	parent := t.TempDir()
	real, link := filepath.Join(parent, "real"), filepath.Join(parent, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	server := newServer(t, faux.New(faux.Options{}), nil, permission.ModeBypass, nil)
	client := connect(t, server)
	id := newSession(t, client, link)
	call(t, client, "session/load", map[string]any{"sessionId": id, "cwd": link, "mcpServers": []any{}}, nil)
}
