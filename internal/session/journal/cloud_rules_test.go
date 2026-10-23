package journal

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/work"
)

func taskStarted(s *Store, jobID, child string, environment *JobEnvironment) JobStarted {
	return JobStarted{JobID: jobID, IdentityDigest: strings.Repeat("a", 64), CallID: "message:" + jobID, ToolCallID: "raw-" + jobID, LogicalRunID: "root", Kind: "task", Spec: json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), Environment: environment, ChildSessionID: child, ChildJournal: s.Path() + ".d/children/" + child + "/session.jsonl", Cwd: "/tmp"}
}

func TestJobCeilingCloudRulesSurviveReload(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	appendAll(t, s, RunStartedRecord{Meta: Meta{RunID: "root"}, Model: "m"})
	rules := []cloudcmd.Rule{
		{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "production", Action: "deny"},
	}
	withRules := &JobEnvironment{Model: "m", Provider: "f"}
	withRules.Ceiling.Mode, withRules.Ceiling.Tools, withRules.Ceiling.CloudRules = "acceptEdits", []string{"bash"}, rules
	bgFact(t, s, "root", KindJobStarted, taskStarted(s, "job_rules", "0000000000000011", withRules))
	// A record written before M5 has no cloud_rules key at all.
	raw := `{"job_id":"job_old","identity_digest":"` + strings.Repeat("a", 64) + `","call_id":"message:old","tool_call_id":"raw-old","logical_run_id":"root","kind":"task","spec":{"prompt":"work","subagent_type":"explore"},"environment":{"ceiling":{"mode":"default","tools":["bash"],"overrides":{},"interactive":{}},"model":"m","provider":"f"},"child_session_id":"0000000000000012","child_journal":"` + s.Path() + `.d/children/0000000000000012/session.jsonl","cwd":"/tmp"}`
	if err := s.AppendWorkFact(context.Background(), work.Fact{Kind: string(KindJobStarted), RunID: "root", Payload: json.RawMessage(raw)}); err != nil {
		t.Fatal(err)
	}
	// Rules NormalizeRules rejects never enter the journal.
	invalid := &JobEnvironment{Model: "m", Provider: "f"}
	invalid.Ceiling.CloudRules = []cloudcmd.Rule{{Tool: "terraform", Context: "prod", Action: "allow_write"}}
	payload, err := json.Marshal(taskStarted(s, "job_bad", "0000000000000013", invalid))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendWorkFact(context.Background(), work.Fact{Kind: string(KindJobStarted), RunID: "root", Payload: payload}); err == nil {
		t.Fatal("journal accepted invalid cloud rules")
	}
	closeStore(t, s)

	loaded, err := Load(context.Background(), s.Path())
	if err != nil || len(loaded.Recoveries) != 2 {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	byID := map[string]work.Recovery{}
	for _, recovery := range loaded.Recoveries {
		byID[recovery.Job.ID] = recovery
	}
	if got := byID["job_rules"].Environment.Ceiling.CloudRules; !slices.Equal(got, rules) {
		t.Fatalf("restored rules = %+v", got)
	}
	if got := byID["job_old"].Environment.Ceiling.CloudRules; got != nil {
		t.Fatalf("old record restored rules = %+v", got)
	}
}
