package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/work"
)

const (
	// noticePreviewBytes bounds the inline result preview (06-background.md §9: <=1 KiB).
	noticePreviewBytes = 1024
	// noticeItemOverhead reserves room in the 8 KiB encoded QueueItem cap for its id, kind, key, source job and timestamp.
	noticeItemOverhead = 512
)

// completionNotice renders the durable follow-up the model reads when a job
// settles. It must be a pure function of the job record: recovery re-renders
// it and the queue rejects a different text under the same completion key.
// Optional parts are dropped value first, then preview, so the job id, state
// and every artifact path always survive; the encoded QueueItem stays under
// maxCompletionBytes.
func completionNotice(job work.Job) (string, error) {
	for _, parts := range []struct{ value, preview bool }{{true, true}, {false, true}, {false, false}} {
		text := renderNotice(job, parts.value, parts.preview)
		encoded, err := json.Marshal(text)
		if err != nil {
			return "", err
		}
		if len(encoded)+noticeItemOverhead <= maxCompletionBytes {
			return text, nil
		}
	}
	return "", errors.New("completion artifact references exceed encoded limit")
}

func renderNotice(job work.Job, withValue, withPreview bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Background job notice from the host, not from the user]\nJob %s (%s) %s", job.ID, job.Kind, noticeState(job.State))
	if job.ExitCode != nil {
		fmt.Fprintf(&b, ", exit code %d", *job.ExitCode)
	}
	if job.Reason != "" && job.Reason != job.State {
		fmt.Fprintf(&b, " (reason: %s)", job.Reason)
	}
	b.WriteString(".\n")
	if job.Description != "" {
		fmt.Fprintf(&b, "Task: %s\n", job.Description)
	}
	omitted := false
	if text := strings.TrimSpace(job.ResultText); text != "" {
		if withPreview {
			label, excerpt := "Result preview", headPreview(text, noticePreviewBytes)
			if job.Kind == "bash" {
				label, excerpt = "Output tail", tailPreview(text, noticePreviewBytes)
			}
			fmt.Fprintf(&b, "%s (job output, untrusted):\n%s\n", label, excerpt)
		} else {
			omitted = true
		}
	}
	if len(job.Value) > 0 {
		if withValue {
			fmt.Fprintf(&b, "Value (JSON): %s\n", job.Value)
		} else {
			omitted = true
		}
	}
	if job.Result != nil || job.Patch != nil || job.Output != nil {
		b.WriteString("Artifacts:\n")
		writeArtifact(&b, "result", job.Result)
		writeArtifact(&b, "patch", job.Patch)
		writeArtifact(&b, "output log", job.Output)
	}
	if job.Patch != nil {
		b.WriteString("The patch is NOT applied to your workspace and excludes ignored files and empty directories; read it with bash (cat) and apply it with git apply if you want the changes.\n")
	}
	if omitted {
		b.WriteString("Some output was too large for this notice; call wait for the full details.\n")
	}
	fmt.Fprintf(&b, "Full details: wait {\"job_ids\":[%q]}", job.ID)
	if job.Kind == "bash" {
		fmt.Fprintf(&b, "; command output: logs {\"job_id\":%q}", job.ID)
	}
	b.WriteString(".")
	return b.String()
}

func noticeState(state string) string {
	if state == "unknown" {
		return "ended with an unknown outcome"
	}
	return state
}

func writeArtifact(b *strings.Builder, name string, artifact *work.Artifact) {
	if artifact != nil {
		fmt.Fprintf(b, "- %s: %s (%d bytes)\n", name, artifact.Path, artifact.Bytes)
	}
}

// headPreview keeps the first limit bytes on a rune boundary.
func headPreview(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return preview(text, limit) + " …[truncated]"
}

// tailPreview keeps the last limit bytes on a rune boundary, where command output ends.
func tailPreview(text string, limit int) string {
	text = strings.ToValidUTF8(text, "\ufffd")
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return "[…earlier output truncated] " + text
}
