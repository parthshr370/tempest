package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"go.harness.dev/harness/tempest"
)

var errStreamGap = errors.New("stream incomplete: updates skipped (tempest/gap)")

func executeOneShot(ctx context.Context, sess *tempest.Session, prompt tempest.Prompt, mode string, stdout, stderr io.Writer, buffer int, requestIDs ...string) (runOutcome, error) {
	if len(requestIDs) > 0 && requestIDs[0] != "" {
		return executeSubmission(ctx, sess, prompt, requestIDs[0], mode, stdout, stderr, buffer)
	}
	return executeOneShotRun(ctx, sess, &prompt, mode, stdout, stderr, buffer)
}

func executeContinuation(ctx context.Context, sess *tempest.Session, mode string, stdout, stderr io.Writer, buffer int) (runOutcome, error) {
	return executeOneShotRun(ctx, sess, nil, mode, stdout, stderr, buffer)
}

func executeOneShotRun(ctx context.Context, sess *tempest.Session, prompt *tempest.Prompt, mode string, stdout, stderr io.Writer, buffer int) (runOutcome, error) {
	var updates <-chan tempest.Update
	stop := func() {}
	if mode == "stream-json" {
		_, updates, stop = sess.Watch(context.Background(), tempest.WatchOptions{Buffer: buffer})
	}
	defer stop()
	var run *tempest.Run
	var err error
	if prompt == nil {
		run, err = sess.BeginContinue(ctx)
	} else {
		run, err = sess.Begin(ctx, *prompt)
	}
	if err != nil {
		return runOutcome{}, err
	}
	stopCancellation := context.AfterFunc(ctx, run.Cancel)
	defer stopCancellation()
	var written chan error
	if updates != nil {
		written = make(chan error, 1)
		go func() {
			encoder := json.NewEncoder(stdout)
			for update := range updates {
				if err := encoder.Encode(update); err != nil {
					run.Cancel()
					written <- err
					return
				}
				if update.Kind == "tempest/gap" {
					written <- errStreamGap
					return
				}
				if update.Kind == "tempest/run" {
					var state struct {
						Status string `json:"status"`
					}
					if json.Unmarshal(update.Update, &state) == nil && state.Status == "finished" {
						written <- nil
						return
					}
				}
			}
			written <- nil
		}()
	}
	result, waitErr := run.Wait()
	outcome := runOutcome{RunResult: result, SessionID: sess.ID(), Elapsed: time.Duration(result.ElapsedMS) * time.Millisecond, LastAssistantText: run.Text()}
	// A setup failure has no durable run, so it has no terminal event/report.
	if result.RunID == "" {
		stop()
		if written != nil {
			<-written
		}
		if waitErr != nil {
			return outcome, waitErr
		}
		if result.Err != nil {
			return outcome, result.Err
		}
		return outcome, errors.New("run ended before durable start")
	}
	if waitErr != nil && outcome.Err == nil {
		outcome.Err = waitErr
	}
	var streamErr error
	if written != nil {
		streamErr = <-written
	}
	if streamErr != nil && !errors.Is(streamErr, errStreamGap) {
		return outcome, streamErr
	}
	if mode == "stream-json" {
		if err := json.NewEncoder(stdout).Encode(makeRunReport(outcome, run.LastMessage())); err != nil {
			return outcome, err
		}
	} else {
		var messages []tempest.Message
		if last := run.LastMessage(); last != nil {
			messages = []tempest.Message{last}
		}
		writeRunOutput(stdout, stderr, mode, outcome, messages)
	}
	if streamErr != nil {
		fmt.Fprintln(stderr, streamErr)
	}
	return outcome, streamErr
}

func executeSubmission(ctx context.Context, sess *tempest.Session, prompt tempest.Prompt, requestID, mode string, stdout, stderr io.Writer, buffer int) (runOutcome, error) {
	_, updates, stop := sess.Watch(context.Background(), tempest.WatchOptions{Buffer: buffer})
	defer stop()
	submission, err := sess.Submit(ctx, tempest.Input{RequestID: requestID, Prompt: prompt, Placement: tempest.PlacementInput})
	if err != nil {
		return runOutcome{}, err
	}
	if mode == "text" {
		fmt.Fprintf(stderr, "session: %s, submission: %s, request: %s\n", sess.ID(), submission.ID, submission.RequestID)
	}
	if submission.State == "accepted" {
		if err := sess.ResumePending(ctx); err != nil && !errors.Is(err, tempest.ErrRunActive) {
			return runOutcome{}, err
		}
	}
	type waited struct {
		value tempest.Submission
		err   error
	}
	done := make(chan waited, 1)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		value, err := sess.WaitSubmission(waitCtx, requestID)
		if err != nil && ctx.Err() != nil {
			sess.Cancel()
			value, err = sess.WaitSubmission(context.Background(), requestID)
		}
		done <- waited{value, err}
	}()
	var streamErr error
	emit := func(u tempest.Update) error {
		if mode != "stream-json" {
			return nil
		}
		if u.Kind == "tempest/gap" {
			streamErr = errStreamGap
		}
		return json.NewEncoder(stdout).Encode(u)
	}
	for {
		select {
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if err := emit(u); err != nil {
				return runOutcome{}, err
			}
		case settled := <-done:
			if settled.err != nil {
				return runOutcome{}, settled.err
			}
			result := settled.value.Result
			outcome := runOutcome{RunResult: result, SessionID: sess.ID(), Elapsed: time.Duration(result.ElapsedMS) * time.Millisecond, LastAssistantText: settled.value.Answer}
			outcome.SubmissionID, outcome.RequestID = settled.value.ID, settled.value.RequestID
			for updates != nil {
				select {
				case u, ok := <-updates:
					if !ok {
						updates = nil
					} else if err := emit(u); err != nil {
						return outcome, err
					}
				default:
					updates = nil
				}
			}
			if mode == "stream-json" {
				if err := json.NewEncoder(stdout).Encode(makeRunReport(outcome, nil)); err != nil {
					return outcome, err
				}
			} else {
				writeRunOutput(stdout, stderr, mode, outcome, nil)
			}
			return outcome, streamErr
		}
	}
}
