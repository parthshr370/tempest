package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/tempest/environment"
)

type inspectionTestLease struct {
	calls int
}

func (l *inspectionTestLease) Reference() environment.Reference {
	return environment.Reference{ID: "remote", Generation: "1", Cwd: "."}
}
func (*inspectionTestLease) Capabilities() environment.Capabilities { return nil }
func (l *inspectionTestLease) Call(context.Context, environment.Call) (json.RawMessage, error) {
	l.calls++
	return json.RawMessage(`{}`), nil
}
func (*inspectionTestLease) Cancel(context.Context, string) (environment.Cancellation, error) {
	return environment.Cancellation{}, nil
}
func (*inspectionTestLease) Lookup(context.Context, string) (environment.Outcome, error) {
	return environment.Outcome{}, nil
}
func (*inspectionTestLease) Close() error { return nil }

func TestInspectEnvironmentSkipsHostOnlyToolsButFailsClosedForWorkspaceTools(t *testing.T) {
	for _, name := range []string{"todo_write", "task", "message", "mcp__server__tool", "ext__bundle__tool"} {
		lease := &inspectionTestLease{}
		ctx, err := InspectEnvironment(context.Background(), lease, name, json.RawMessage(`{}`))
		if err != nil || lease.calls != 0 {
			t.Fatalf("%s inspection: calls=%d err=%v", name, lease.calls, err)
		}
		if _, ok := ctx.Value(workspaceInspectionKey{}).(WorkspaceInspection); !ok {
			t.Fatalf("%s did not receive host-only inspection facts", name)
		}
	}
	lease := &inspectionTestLease{}
	if _, err := InspectEnvironment(context.Background(), lease, "edit", json.RawMessage(`{}`)); !errors.Is(err, environment.ErrUnsupported) {
		t.Fatalf("workspace tool without inspection capability error = %v", err)
	}
	if lease.calls != 0 {
		t.Fatalf("workspace tool unexpectedly dispatched inspection: calls=%d", lease.calls)
	}
}
func TestRemoteInspectionCannotOverrideHostCommandParsing(t *testing.T) {
	args := json.RawMessage(`{"command":"rm -rf /tmp/nonexistent-tempest-review-target"}`)
	ctx := context.WithValue(context.Background(), workspaceInspectionKey{}, WorkspaceInspection{
		Bash:     true,
		Command:  "echo safe",
		Analysis: cloudcmd.Analysis{Effect: cloudcmd.EffectRead, Proven: true},
	})
	facts, _ := inspectWorkspaceForGate(ctx, "bash", args, t.TempDir())
	if facts.command != "rm -rf /tmp/nonexistent-tempest-review-target" || facts.guardReason == "" || facts.analysis.Effect == cloudcmd.EffectRead {
		t.Fatalf("remote inspection overrode host command analysis: %+v", facts)
	}
}
