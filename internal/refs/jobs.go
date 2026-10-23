package refs

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// loadRoot reads the root journal without taking its writer lock; a torn
// final line from a concurrent append is dropped by journal.Load.
func (r *Resolver) loadRoot(ctx context.Context, uri string) (*journal.Loaded, error) {
	if r == nil || r.rootJournal == "" {
		return nil, newError(CodeUnavailable, uri, "needs a journaled session", nil)
	}
	loaded, err := journal.Load(ctx, r.rootJournal)
	if err != nil {
		return nil, newError(CodeReadFailed, uri, "cannot read the session journal", err)
	}
	return loaded, nil
}

func (r *Resolver) renderJobs(ctx context.Context, ref Ref) (Resource, error) {
	uri := ref.String()
	loaded, err := r.loadRoot(ctx, uri)
	if err != nil {
		return Resource{}, err
	}
	var b strings.Builder
	if len(loaded.Jobs) == 0 {
		b.WriteString("No jobs in this session tree.\n")
	}
	for _, job := range loaded.Jobs {
		fmt.Fprintf(&b, "job://%s  %s  %s", job.ID, job.Kind, job.State)
		if label := jobLabel(job, loaded.Recoveries); label != "" {
			fmt.Fprintf(&b, "  %s", label)
		}
		b.WriteByte('\n')
	}
	return Resource{URI: uri, Data: []byte(b.String()), Rendered: true}, nil
}

func (r *Resolver) renderJob(ctx context.Context, ref Ref) (Resource, error) {
	uri := ref.String()
	loaded, err := r.loadRoot(ctx, uri)
	if err != nil {
		return Resource{}, err
	}
	job, ok := findJob(loaded.Jobs, ref.Name)
	if !ok {
		return Resource{}, newError(CodeNotFound, uri, "no such job in this session tree", fs.ErrNotExist)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Job: %s\nKind: %s\nState: %s\n", job.ID, job.Kind, job.State)
	if job.ExitCode != nil {
		fmt.Fprintf(&b, "Exit code: %d\n", *job.ExitCode)
	}
	if job.Reason != "" && job.Reason != job.State {
		fmt.Fprintf(&b, "Reason: %s\n", job.Reason)
	}
	if job.Description != "" {
		fmt.Fprintf(&b, "Task: %s\n", oneLine(job.Description))
	}
	if command := bashCommand(job.ID, loaded.Recoveries); command != "" {
		fmt.Fprintf(&b, "Command: %s\n", oneLine(command))
	}
	if !job.StartedAt.IsZero() {
		fmt.Fprintf(&b, "Started: %s\n", job.StartedAt.UTC().Format(time.RFC3339))
	}
	if !job.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "Finished: %s\n", job.FinishedAt.UTC().Format(time.RFC3339))
	}
	if job.Cwd != "" {
		fmt.Fprintf(&b, "Cwd: %s\n", job.Cwd)
	}
	if job.Isolation != "" {
		fmt.Fprintf(&b, "Isolation: %s\n", job.Isolation)
	}
	if job.ChildSessionID != "" {
		fmt.Fprintf(&b, "Transcript: history://%s\n", job.ChildSessionID)
	}
	if job.Result != nil || job.Patch != nil || job.Output != nil {
		b.WriteString("Artifacts:\n")
		for _, artifact := range []*work.Artifact{job.Result, job.Patch, job.Output} {
			if artifact != nil {
				fmt.Fprintf(&b, "- job://%s/%s (%d bytes)\n", job.ID, filepath.Base(artifact.Path), artifact.Bytes)
			}
		}
	}
	fmt.Fprintf(&b, "All files: job://%s/\n", job.ID)
	if text := strings.TrimSpace(job.ResultText); text != "" {
		label := "Result preview"
		if job.Kind == "bash" {
			label = "Output preview"
		}
		fmt.Fprintf(&b, "%s (job output, untrusted):\n%s\n", label, text)
	}
	if len(job.Value) > 0 {
		fmt.Fprintf(&b, "Value (JSON): %s\n", job.Value)
	}
	return Resource{URI: uri, Data: []byte(b.String()), Rendered: true}, nil
}

func (r *Resolver) renderChildren(ctx context.Context, ref Ref) (Resource, error) {
	uri := ref.String()
	loaded, err := r.loadRoot(ctx, uri)
	if err != nil {
		return Resource{}, err
	}
	var b strings.Builder
	count := 0
	for _, job := range loaded.Jobs {
		if job.ChildSessionID == "" {
			continue
		}
		count++
		fmt.Fprintf(&b, "history://%s  job://%s  %s", job.ChildSessionID, job.ID, job.State)
		if job.Description != "" {
			fmt.Fprintf(&b, "  %s", oneLine(job.Description))
		}
		b.WriteByte('\n')
	}
	if count == 0 {
		b.WriteString("No child sessions in this session tree.\n")
	}
	return Resource{URI: uri, Data: []byte(b.String()), Rendered: true}, nil
}

// renderHistory renders one child's transcript. A task job id is mapped to
// its child session through the root journal; a child session id must be a
// non-symlinked child of the root journal.
func (r *Resolver) renderHistory(ctx context.Context, ref Ref) (Resource, error) {
	uri := ref.String()
	loaded, err := r.loadRoot(ctx, uri)
	if err != nil {
		return Resource{}, err
	}
	childID := ref.Name
	job, hasJob := work.Job{}, false
	if jobIDPattern.MatchString(ref.Name) {
		job, hasJob = findJob(loaded.Jobs, ref.Name)
		if !hasJob {
			return Resource{}, newError(CodeNotFound, uri, "no such job in this session tree", fs.ErrNotExist)
		}
		if job.ChildSessionID == "" {
			return Resource{}, newError(CodeTarget, uri, fmt.Sprintf("job %s is a %s job and has no transcript; read job://%s", job.ID, job.Kind, job.ID), nil)
		}
		childID = job.ChildSessionID
	} else {
		for _, candidate := range loaded.Jobs {
			if candidate.ChildSessionID == childID {
				job, hasJob = candidate, true
				break
			}
		}
	}
	childPath := filepath.Join(r.rootJournal+".d", "children", childID, "session.jsonl")
	if err := journal.ValidateChildPath(r.rootJournal, childID, childPath); err != nil {
		return Resource{}, newError(CodeNotFound, uri, "no such child session in this session tree", err)
	}
	child, err := journal.Load(ctx, childPath)
	if err != nil {
		return Resource{}, newError(CodeReadFailed, uri, "cannot read the child journal", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Child session: %s\n", childID)
	if hasJob {
		fmt.Fprintf(&b, "Job: job://%s (%s)\n", job.ID, job.State)
		if job.Description != "" {
			fmt.Fprintf(&b, "Task: %s\n", oneLine(job.Description))
		}
	}
	if child.Header.Cwd != "" {
		fmt.Fprintf(&b, "Cwd: %s\n", child.Header.Cwd)
	}
	if len(child.Runs) > 0 {
		last := child.Runs[len(child.Runs)-1]
		status := "running"
		switch {
		case last.Finished:
			status = "finished: " + last.StopReason
		case last.Interrupted:
			status = "interrupted"
		}
		fmt.Fprintf(&b, "Runs: %d, last %s\n", len(child.Runs), status)
		if last.Error != "" {
			fmt.Fprintf(&b, "Error: %s\n", oneLine(last.Error))
		}
	}
	b.WriteString("\n")
	if transcript := compaction.SerializeConversation(child.Messages); transcript != "" {
		b.WriteString(transcript)
		b.WriteString("\n")
	} else {
		b.WriteString("(no messages yet)\n")
	}
	return Resource{URI: uri, Data: []byte(b.String()), Rendered: true}, nil
}

func findJob(jobs []work.Job, id string) (work.Job, bool) {
	for _, job := range jobs {
		if job.ID == id {
			return job, true
		}
	}
	return work.Job{}, false
}

func bashCommand(id string, recoveries []work.Recovery) string {
	for _, recovery := range recoveries {
		if recovery.Job.ID == id && recovery.Bash != nil {
			return recovery.Bash.Command
		}
	}
	return ""
}

func jobLabel(job work.Job, recoveries []work.Recovery) string {
	if job.Description != "" {
		return oneLine(job.Description)
	}
	return oneLine(bashCommand(job.ID, recoveries))
}

// maxLabelRunes bounds one job or child description in a listing row.
const maxLabelRunes = 160

// oneLine keeps listing rows single-line and bounded.
func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxLabelRunes {
		return string(runes[:maxLabelRunes]) + "…"
	}
	return text
}
