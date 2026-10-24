package extension_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/tempest/extension"
)

func TestProcessTaskMigrationHasNoCallbackAuthority(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	fixture := filepath.Join(t.TempDir(), "migration.py")
	script := `import json,sys
for line in sys.stdin:
 frame=json.loads(line)
 if 'id' not in frame: continue
 method=frame['method']
 if method=='initialize':
  result={'protocolVersion':'tempest-extension/1','id':'tasks','revision':'v2','definition':{'tasks':[{'name':'flow','version':2,'inputSchema':{'type':'object'},'stateSchema':{'type':'object'},'initial':{},'migrations':[1]}]}}
 elif method=='_tempest/extension/tasks/step':
  params=frame['params']
  assert 'context' not in params and 'task' not in params
  assert params=={'name':'flow','migrate':{'fromVersion':1,'checkpoint':{'old':True}}}
  result={'checkpoint':{'migrated':True}}
 elif method=='_tempest/extension/shutdown': result={}
 else: raise RuntimeError(method)
 print(json.dumps({'jsonrpc':'2.0','id':frame['id'],'result':result}),flush=True)
`
	if err = os.WriteFile(fixture, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	if err = registry.InstallProcess(t.Context(), extension.ProcessOptions{ID: "tasks", Argv: []string{python, fixture}, Capabilities: []string{"tasks"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Acquire([]extension.Selection{{ID: "tasks", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(context.Background())
	definition := lease.Definitions()[0].Tasks[0]
	state, err := definition.Migrate(1, json.RawMessage(`{"old":true}`))
	if err != nil || string(state) != `{"migrated": true}` {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if _, err = definition.Migrate(99, json.RawMessage(`{}`)); err == nil {
		t.Fatal("unsupported source version reached peer")
	}
}
