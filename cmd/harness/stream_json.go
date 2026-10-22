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

func executeOneShot(ctx context.Context, sess *tempest.Session, prompt, mode string, stdout, stderr io.Writer, buffer int) (runOutcome, error) {
	var updates <-chan tempest.Update
	stop := func() {}
	if mode == "stream-json" {
		_, updates, stop = sess.Watch(context.Background(), tempest.WatchOptions{Buffer: buffer})
	}
	defer stop()
	run, err := sess.Begin(ctx, tempest.Prompt{Text: prompt})
	if err != nil {
		return runOutcome{}, err
	}
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
