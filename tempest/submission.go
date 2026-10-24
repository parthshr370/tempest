package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

// Placement selects direct input or the existing checkpoint queues.
type Placement string

const (
	// PlacementInput starts a new generation and rejects a busy session.
	PlacementInput Placement = "input"
	// PlacementSteer delivers input at the next steering checkpoint.
	PlacementSteer Placement = "steer"
	// PlacementFollowUp delivers input after the current turn.
	PlacementFollowUp Placement = "follow_up"
)

// Input identifies original input independently of a transport request ID.
type Input struct {
	RequestID string    `json:"requestId"`
	Prompt    Prompt    `json:"prompt"`
	Placement Placement `json:"placement"`
}

// Submission is a durable logical input, not an exactly-once provider charge.
type Submission struct {
	ID            string    `json:"id"`
	RequestID     string    `json:"requestId"`
	State         string    `json:"state"`
	RunID         string    `json:"runId,omitempty"`
	AnswerEntryID string    `json:"answerEntryId,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Revision      uint64    `json:"revision"`
	Answer        string    `json:"answer,omitempty"`
	Result        RunResult `json:"result"`
	Error         string    `json:"error,omitempty"`
}

func publicSubmission(v durable.Submission) Submission {
	out := Submission{ID: v.ID, RequestID: v.RequestID, State: v.State, RunID: v.RunID, AnswerEntryID: v.AnswerEntryID, Reason: v.Reason, Revision: v.Revision, Answer: v.Answer, Error: v.Error}
	_ = json.Unmarshal(v.Result, &out.Result)
	if v.Error != "" {
		out.Result.Err = errors.New(v.Error)
		if out.Result.StopReason == "" {
			out.Result.StopReason = agent.StopError
		}
	}
	if out.Result.StopReason == "" {
		switch v.State {
		case "withdrawn":
			out.Result.StopReason = agent.StopAborted
		case "blocked", "unknown", "failed":
			out.Result.StopReason = agent.StopError
			out.Result.Err = errors.New(v.Reason)
		case "completed":
			out.Result.StopReason = agent.StopCompleted
		}
	}
	if out.Result.RunID == "" {
		out.Result.RunID = v.RunID
	}
	return out
}
func (s *Session) submissionRecord(ctx context.Context, id string) (durable.Submission, error) {
	p, err := s.core.store.Read(ctx, durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: id, Limit: 1})
	if err != nil {
		return durable.Submission{}, err
	}
	if len(p.Records) == 0 {
		return durable.Submission{}, ErrQueueNotFound
	}
	var v durable.Submission
	if err = json.Unmarshal(p.Records[0].Value, &v); err != nil {
		return v, err
	}
	v.Revision = p.Records[0].Revision
	return v, nil
}
func (s *Session) saveSubmission(ctx context.Context, v durable.Submission, decision string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	key := durable.SubmissionKey(v.ClientNamespace, v.RequestID)
	_, err = s.core.store.Commit(ctx, durable.Commit{ID: decision, Expected: []durable.RevisionCheck{{SessionID: s.ID(), Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key, Revision: v.Revision}}, Changes: []durable.Change{{SessionID: s.ID(), Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key, Version: 1, Value: raw}}})
	if err == nil {
		s.publishDurable(context.WithoutCancel(ctx))
	}
	return err
}

// Submit fsyncs admission and scheduling intent before success. Disconnecting
// cancels the wait, not accepted input. Reusing an ID returns its original work.
func (s *Session) Submit(ctx context.Context, in Input) (Submission, error) {
	return s.submit(ctx, in, false)
}

func (s *Session) submit(ctx context.Context, in Input, bindContext bool) (Submission, error) {
	if err := s.authorize(ctx, "submit", "", in.RequestID); err != nil {
		return Submission{}, err
	}
	var contextErr error
	ctx, contextErr = s.submissionContext(ctx)
	if contextErr != nil {
		return Submission{}, contextErr
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), in.RequestID)
	if in.Placement == "" {
		in.Placement = PlacementInput
	}
	if in.RequestID == "" || len(in.RequestID) > 128 || strings.ContainsRune(in.RequestID, 0) {
		return Submission{}, ErrInvalidQueueInput
	}
	raw, err := json.Marshal(in.Prompt)
	if err != nil {
		return Submission{}, err
	}
	hash, err := durable.SubmissionHash(string(in.Placement), raw)
	if err != nil {
		return Submission{}, err
	}
	c := s.core
	c.admission.Lock()
	defer c.admission.Unlock()
	prior, err := s.submissionRecord(ctx, key)
	if err == nil {
		if prior.RequestHash != hash {
			return Submission{}, durable.ErrConflict
		}
		return publicSubmission(prior), nil
	}
	if !errors.Is(err, ErrQueueNotFound) {
		return Submission{}, err
	}
	if _, err := outputschema.Compile(in.Prompt.OutputSchema); err != nil {
		return Submission{}, fmt.Errorf("%w: %v", ErrInvalidOutputSchema, err)
	}
	if in.Placement != PlacementInput {
		if len(in.Prompt.Media) != 0 || len(in.Prompt.OutputSchema) != 0 {
			return Submission{}, ErrInvalidQueueInput
		}
		kind := work.QueueKind(in.Placement)
		item, e := c.queues.EnqueuePrompt(ctx, work.QueueInput{Kind: kind, Text: in.Prompt.Text, Key: in.RequestID}, raw)
		if e != nil {
			return Submission{}, e
		}
		v, e := s.submissionRecord(ctx, key)
		if e != nil {
			return Submission{}, e
		}
		v.QueueID = item.ID
		return publicSubmission(v), nil
	}
	c.mu.Lock()
	closed, busy := c.closed, c.active != nil
	c.mu.Unlock()
	if closed {
		return Submission{}, ErrClosed
	}
	if busy {
		return Submission{}, ErrRunActive
	}
	if c.jobs.RecoveryRequired() {
		return Submission{}, ErrRecoveryRequired
	}
	var release func()
	if c.admitRun != nil {
		release, err = c.admitRun(ctx)
		if err != nil {
			return Submission{}, err
		}
	}
	transferred := false
	defer func() {
		if !transferred && release != nil {
			release()
		}
	}()
	v := durable.Submission{ID: c.store.NewRunID(), RequestID: in.RequestID, ClientNamespace: durable.ClientNamespace(ctx), State: "accepted", RunID: c.store.NewRunID(), RequestHash: hash, Placement: string(in.Placement), Payload: raw}
	if err = s.saveSubmission(ctx, v, "admit:"+key); err != nil {
		return Submission{}, err
	}
	v.Revision = 1
	transferred = true
	runContext := context.WithoutCancel(ctx)
	if bindContext {
		runContext = ctx
	}
	_, e := s.start(runContext, &in.Prompt, key, release)
	if e != nil {
		v.State = "blocked"
		v.Reason = e.Error()
		v.Error = e.Error()
		if err = s.saveSubmission(context.WithoutCancel(ctx), v, "blocked:"+key); err != nil {
			return Submission{}, err
		}
		v.Revision++
		return Submission{}, e
	}
	current, err := s.submissionRecord(context.WithoutCancel(ctx), key)
	if err != nil {
		return Submission{}, err
	}
	return publicSubmission(current), nil
}

// Submission looks up a lifetime receipt by its caller-provided request ID.
func (s *Session) Submission(ctx context.Context, id string) (Submission, error) {
	if err := s.authorize(ctx, "get_submission", "", id); err != nil {
		return Submission{}, err
	}
	ctx, err := s.submissionContext(ctx)
	if err != nil {
		return Submission{}, err
	}
	v, err := s.submissionRecord(ctx, durable.SubmissionKey(durable.ClientNamespace(ctx), id))
	if err != nil {
		return Submission{}, err
	}
	return publicSubmission(v), nil
}
func terminalSubmission(state string) bool {
	switch state {
	case "completed", "failed", "blocked", "withdrawn", "unknown":
		return true
	}
	return false
}

// WaitSubmission cancels only this wait and can be repeated after reconnect.
func (s *Session) WaitSubmission(ctx context.Context, id string) (Submission, error) {
	var err error
	ctx, err = s.submissionContext(ctx)
	if err != nil {
		return Submission{}, err
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), id)
	for {
		changed := s.core.submissionSignal()
		v, err := s.Submission(ctx, id)
		if err != nil {
			return Submission{}, err
		}
		c := s.core
		c.mu.Lock()
		active := c.active != nil
		r := c.submissionRuns[key]
		if r == nil && c.active != nil && v.RunID != "" && (c.active.startID == v.RunID || c.active.admittedRunID == v.RunID) {
			r = c.active
		}
		c.mu.Unlock()
		if r != nil {
			select {
			case <-ctx.Done():
				return Submission{}, ctx.Err()
			case <-r.done:
				if r.submission != nil && r.requestID == key {
					return *r.submission, nil
				}
				continue
			}
		}
		if terminalSubmission(v.State) {
			return v, nil
		}
		if r == nil && !active && (v.State == "running" || v.State == "consumed") {
			stored, err := s.submissionRecord(ctx, key)
			if err != nil {
				return Submission{}, err
			}
			if stored.State != "running" && stored.State != "consumed" {
				continue
			}
			stored.State = "unknown"
			stored.Reason = "process stopped after dispatch; outcome unknown"
			stored.Error = stored.Reason
			stored.Result, _ = json.Marshal(RunResult{RunID: stored.RunID, StopReason: agent.StopError})
			if err = s.saveSubmission(ctx, stored, fmt.Sprintf("unknown:%s:%d", key, stored.Revision)); err != nil {
				if errors.Is(err, durable.ErrConflict) {
					continue
				}
				return Submission{}, err
			}
			stored.Revision++
			return publicSubmission(stored), nil
		}
		select {
		case <-ctx.Done():
			return Submission{}, ctx.Err()
		case <-changed:
		}
	}
}

// Withdraw removes only accepted, unclaimed work. It never reopens on retry.
func (s *Session) Withdraw(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "withdraw", "", id); err != nil {
		return err
	}
	var err error
	ctx, err = s.submissionContext(ctx)
	if err != nil {
		return err
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), id)
	c := s.core
	c.admission.Lock()
	defer c.admission.Unlock()
	v, err := s.submissionRecord(ctx, key)
	if err != nil {
		return err
	}
	if v.State == "withdrawn" {
		return nil
	}
	if terminalSubmission(v.State) || v.State == "running" || v.State == "consumed" {
		return ErrQueueTooLate
	}
	if v.QueueID != "" {
		return c.queues.Cancel(ctx, v.QueueID)
	}
	v.State = "withdrawn"
	return s.saveSubmission(ctx, v, "withdraw:"+key)
}

func (s *Session) begin(ctx context.Context, p *Prompt) (*Run, error) {
	if p == nil {
		if err := s.authorize(ctx, "resume_pending", "", ""); err != nil {
			return nil, err
		}
		c := s.core
		c.mu.Lock()
		busy := c.active != nil
		c.mu.Unlock()
		if busy {
			return nil, ErrRunActive
		}
		r, err := s.resumePending(ctx)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return r, nil
		}
		c.admission.Lock()
		defer c.admission.Unlock()
		return s.start(ctx, nil, "", nil)
	}
	var err error
	ctx, err = s.submissionContext(ctx)
	if err != nil {
		return nil, err
	}
	id := s.core.store.NewRunID()
	v, err := s.submit(ctx, Input{RequestID: id, Prompt: *p, Placement: PlacementInput}, true)
	if err != nil {
		return nil, err
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), id)
	s.core.mu.Lock()
	r := s.core.submissionRuns[key]
	s.core.mu.Unlock()
	if r == nil {
		settled, err := s.Submission(ctx, id)
		if err != nil {
			return nil, err
		}
		if !terminalSubmission(settled.State) {
			return nil, fmt.Errorf("submission %s: %s", v.State, v.Reason)
		}
		r = &Run{cancel: func() {}, done: make(chan struct{}), ready: make(chan struct{}), result: settled.Result, err: settled.Result.Err, text: settled.Answer, startID: settled.RunID}
		close(r.ready)
		close(r.done)
	}
	return r, nil
}

func (s *Session) finishSubmission(r *Run) {
	if r.requestID == "" {
		return
	}
	ctx := context.Background()
	v, err := s.submissionRecord(ctx, r.requestID)
	if err != nil {
		r.err = errors.Join(r.err, err)
		return
	}
	v.State = "completed"
	v.Answer = r.text
	v.RunID = r.startID
	result := r.result
	result.Err = nil
	v.Result, _ = json.Marshal(result)
	terminalErr := errors.Join(r.err, r.result.Err)
	if terminalErr != nil {
		v.State = "failed"
		v.Error = terminalErr.Error()
		v.Reason = v.Error
	}
	if err = s.saveSubmission(ctx, v, fmt.Sprintf("settle:%s:%d", r.requestID, v.Revision)); err != nil {
		r.err = errors.Join(r.err, err)
		return
	}
	v.Revision++
	terminal := publicSubmission(v)
	r.submission = &terminal
}

// ResumePending schedules only persisted accepted input. Interrupted provider
// dispatch remains unknown; it isn't an authorization to repeat paid work.
func (s *Session) ResumePending(ctx context.Context) error {
	_, err := s.resumePending(ctx)
	return err
}

func (s *Session) resumePending(ctx context.Context) (*Run, error) {
	if err := s.authorize(ctx, "resume_pending", "", ""); err != nil {
		return nil, err
	}
	c := s.core
	c.admission.Lock()
	defer c.admission.Unlock()
	c.mu.Lock()
	busy, closed := c.active != nil, c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if busy {
		return nil, ErrRunActive
	}
	if c.resumeExtensions != nil {
		if err := c.resumeExtensions(ctx); err != nil {
			return nil, err
		}
	}
	q := durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Limit: 100, Active: true}
	for {
		page, err := c.store.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			var v durable.Submission
			if err = json.Unmarshal(record.Value, &v); err != nil {
				return nil, err
			}
			if v.Placement == string(PlacementInput) && v.State == "accepted" {
				var p Prompt
				if err = json.Unmarshal(v.Payload, &p); err != nil {
					return nil, err
				}
				// Earlier payload encoders represented an absent schema as null.
				if string(p.OutputSchema) == "null" {
					p.OutputSchema = nil
				}
				return s.start(context.WithoutCancel(ctx), &p, durable.SubmissionKey(v.ClientNamespace, v.RequestID), nil)
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	return nil, nil
}

func (c *sessionCore) submissionSignal() <-chan struct{} {
	c.submissionNotifyMu.Lock()
	defer c.submissionNotifyMu.Unlock()
	if c.submissionChanged == nil {
		c.submissionChanged = make(chan struct{})
	}
	return c.submissionChanged
}
func (c *sessionCore) notifySubmissions() {
	c.submissionNotifyMu.Lock()
	defer c.submissionNotifyMu.Unlock()
	if c.submissionChanged != nil {
		close(c.submissionChanged)
	}
	c.submissionChanged = make(chan struct{})
}

// CancelSubmission cancels only the live run owned by this request.
func (s *Session) CancelSubmission(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "cancel", "", id); err != nil {
		return err
	}
	ctx, err := s.submissionContext(ctx)
	if err != nil {
		return err
	}
	key := durable.SubmissionKey(durable.ClientNamespace(ctx), id)
	c := s.core
	c.admission.Lock()
	defer c.admission.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.requestID != key {
		return ErrQueueTooLate
	}
	c.active.cancel()
	return nil
}
